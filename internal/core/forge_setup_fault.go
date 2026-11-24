package core

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// forgeSetupError is a forge failure that is the forge's setup, not its being
// down: what is said is the cause, and it is an InvalidError underneath, so
// the API answers it as a refusal that trying again doesn't change (WP-1370).
type forgeSetupError struct {
	cause string
	err   error
}

func (e *forgeSetupError) Error() string { return e.cause }
func (e *forgeSetupError) Unwrap() []error {
	return []error{&InvalidError{Msg: e.cause}, e.err}
}

// forgeSetupFault is the one classifier of a forge failure that waiting
// won't mend, and what it says is the cause: a certificate that doesn't
// verify, an address that answers with a redirect (the forge is addressed
// wrongly), and an answer that isn't the forge's API (a login page with 200,
// a proxy's page). "" is any other failure. shipRefused and forgeDown both
// ask it, so the two never disagree about retrying a bad certificate for a
// day.
func forgeSetupFault(err error) string {
	if err == nil {
		return ""
	}
	var fs *forgeSetupError
	if errors.As(err, &fs) {
		return fs.cause
	}
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	if errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci) {
		return "the forge's TLS certificate doesn't verify (" + PublicError(err) + "): fix the certificate or the forge's address, as trying again won't change it"
	}
	var ae *forgeAPIError
	if errors.As(err, &ae) && ae.status >= 300 && ae.status < 400 {
		return fmt.Sprintf("the forge answered %s %s with a redirect (HTTP %d), not its API: its address is probably wrong (http for https, or a moved host)", ae.method, ae.path, ae.status)
	}
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syn) || errors.As(err, &typ) {
		return "the forge answered with something that isn't its API (a login page or a proxy's page, with HTTP 200): check the forge's address and token"
	}
	return ""
}

// forgeRefusalStatus is a status the forge refuses a request with, that
// trying again doesn't change.
func forgeRefusalStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	}
	return false
}
