package broker

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// CA is the sandbox CA: sandboxes trust it, nothing else does, and the
// broker signs a leaf certificate with it for each inspected host.
type CA struct {
	cert    *x509.Certificate
	certDER []byte
	key     crypto.Signer

	leafKey *ecdsa.PrivateKey
	mu      sync.Mutex
	leaves  map[string]*tls.Certificate
	now     func() time.Time
}

const (
	caLife   = 10 * 365 * 24 * time.Hour
	leafLife = 7 * 24 * time.Hour
	// A leaf is replaced once less than this is left of it.
	leafRenew = 24 * time.Hour
)

// NewCA makes a CA. It can sign only leaf certificates (path length 0).
func NewCA(name string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name, Organization: []string{"Waypoint sandbox"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caLife),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return newCA(der, key)
}

// LoadCA reads a CA that MarshalPEM wrote.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || cb.Type != "CERTIFICATE" || kb == nil {
		return nil, errors.New("sandbox CA: not a PEM certificate and key")
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("sandbox CA key: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("sandbox CA key can't sign")
	}
	return newCA(cb.Bytes, signer)
}

func newCA(der []byte, key crypto.Signer) (*CA, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, errors.New("sandbox CA: the certificate isn't a CA")
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, certDER: der, key: key, leafKey: leafKey,
		leaves: map[string]*tls.Certificate{}, now: time.Now}, nil
}

// CertPEM is the certificate sandboxes are told to trust.
func (c *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.certDER})
}

// MarshalPEM is the certificate and its private key, for LoadCA.
func (c *CA) MarshalPEM() (certPEM, keyPEM []byte, err error) {
	kb, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return nil, nil, err
	}
	return c.CertPEM(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// Pool is a pool holding only this CA, for tests and clients.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// Leaf returns a certificate for host, signed by the CA. Leaves are kept
// and reused until they near expiry.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if l, ok := c.leaves[host]; ok && l.Leaf.NotAfter.Sub(now) > leafRenew {
		return l, nil
	}
	notAfter := now.Add(leafLife)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &c.leafKey.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.certDER}, PrivateKey: c.leafKey, Leaf: leaf}
	c.leaves[host] = l
	return l, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
