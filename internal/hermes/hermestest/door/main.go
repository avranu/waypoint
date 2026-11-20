// Command door runs a fake assistant gate for local development and the
// browser checks: HTTPS from a CA of its own (written to -ca), the door's
// key, the gate's control API in memory, and a real Hermes dashboard behind.
//
//	go run ./internal/hermes/hermestest/door -hermes http://127.0.0.1:19119 -key devkey -ca /tmp/door-ca.pem
//
// With -llm ADDR it also serves hermestest.FakeLLM, a scripted model for
// the Hermes behind it (model.base_url: http://ADDR/v1).
//
// POST /dev/ask?summary=… (with the key) adds a pending approval, and
// POST /dev/decide?code=…&approve=1 decides one, as Signal would.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/hermes/hermestest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:19443", "address to serve HTTPS on")
	upstream := flag.String("hermes", "http://127.0.0.1:19119", "the Hermes dashboard to pass requests to")
	key := flag.String("key", "devkey", "the door's control key")
	header := flag.String("header", hermestest.DefaultKeyHeader, "the header the door reads its key from")
	caOut := flag.String("ca", "door-ca.pem", "where to write the door's CA certificate")
	llm := flag.String("llm", "", "also serve a scripted OpenAI-compatible model on this address (/v1)")
	flag.Parse()

	if *llm != "" {
		go func() {
			s := &http.Server{Addr: *llm, Handler: hermestest.FakeLLM(), ReadHeaderTimeout: 10 * time.Second}
			log.Fatal(s.ListenAndServe())
		}()
	}

	hermes, err := hermestest.ProxyTo(*upstream)
	if err != nil {
		log.Fatal(err)
	}
	door := hermestest.NewDoor(*key, hermes)
	door.Header = *header
	mux := http.NewServeMux()
	mux.HandleFunc("POST /dev/ask", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(*header) != *key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		tool := r.URL.Query().Get("tool")
		if tool == "" {
			tool = "send_email"
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(door.Ask(tool, r.URL.Query().Get("summary")))) // #nosec G705 -- a dev tool's plain-text code
	})
	mux.HandleFunc("POST /dev/decide", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(*header) != *key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		door.Decide(r.URL.Query().Get("code"), r.URL.Query().Get("approve") == "1")
	})
	mux.HandleFunc("POST /dev/thaw", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(*header) == *key {
			door.SetFrozen(false)
		}
	})
	mux.Handle("/", door)

	cert, err := certs(*caOut)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	log.Printf("door on https://%s → %s (CA in %s)", *listen, *upstream, *caOut)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// certs makes a CA (or reuses the one at caPath and its key beside it, so
// a restart doesn't change what clients pinned) and a certificate for
// 127.0.0.1 and localhost it signs.
func certs(caPath string) (tls.Certificate, error) {
	keyPath := strings.TrimSuffix(caPath, ".pem") + ".key"
	ca, caKey, err := loadCA(caPath, keyPath)
	if err != nil {
		caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "door dev CA"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), IsCA: true,
			KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &caKey.PublicKey, caKey)
		if err != nil {
			return tls.Certificate{}, err
		}
		ca, _ = x509.ParseCertificate(der)
		kb, _ := x509.MarshalECPrivateKey(caKey)
		if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			return tls.Certificate{}, err
		}
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
			return tls.Certificate{}, err
		}
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "door"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(certPath) // #nosec G304 -- the developer's own -ca flag
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(keyPath) // #nosec G304 -- beside the developer's own -ca file
	if err != nil {
		return nil, nil, err
	}
	cblk, _ := pem.Decode(cb)
	kblk, _ := pem.Decode(kb)
	if cblk == nil || kblk == nil {
		return nil, nil, fmt.Errorf("not PEM")
	}
	ca, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	return ca, key, err
}
