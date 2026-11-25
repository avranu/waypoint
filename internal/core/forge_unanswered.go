package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// forgeTimeout bounds one forge API call made in the background.
	forgeTimeout = 15 * time.Second
	// forgePatientTimeout bounds one made while a person waits on a write
	// they asked for (opening a pull request): a busy forge gets longer
	// before the attempt is given up.
	forgePatientTimeout = 30 * time.Second
)

type forgePatientKey struct{}

// forgePatient marks ctx's forge calls as ones a person waits on; the
// forge client gives them forgePatientTimeout.
func forgePatient(ctx context.Context) context.Context {
	return context.WithValue(ctx, forgePatientKey{}, true)
}

func isForgePatient(ctx context.Context) bool {
	b, _ := ctx.Value(forgePatientKey{}).(bool)
	return b
}

// client is the HTTP client for a call made with ctx.
func (c *httpForgeClient) client(ctx context.Context) *http.Client {
	if c.patient != nil && isForgePatient(ctx) {
		return c.patient
	}
	return c.http
}

// forgeDownError is a forge that didn't answer: it is ErrUnavailable and
// still the transport error underneath. what says what happened, then is
// what to do about it.
type forgeDownError struct {
	what, then string
	err        error
}

func (e *forgeDownError) Error() string   { return e.what + "; " + e.then }
func (e *forgeDownError) Unwrap() []error { return []error{ErrUnavailable, e.err} }

// forgeUnanswered is forgeDown that logs the transport's own words, which
// the message leaves out.
func (s *Service) forgeUnanswered(f *Forge, waited time.Duration, err error) error {
	down := forgeDown(f, waited, err)
	if down != err {
		s.log.Warn("forge didn't answer", "forge", forgeHost(f), "waited", waited.Round(time.Millisecond), "err", err)
	}
	return down
}

// forgeDown returns err as a forgeDownError when it says f didn't answer:
// it couldn't be reached, it timed out after waited, or it answered 502,
// 503 or 504. Anything else (a refusal, a blocked address, a caller that
// went away) is returned as it is.
func forgeDown(f *Forge, waited time.Duration, err error) error {
	if err == nil || errors.Is(err, ErrEgressBlocked) || errors.Is(err, context.Canceled) {
		return err
	}
	var down *forgeDownError
	if errors.As(err, &down) {
		return err
	}
	if cause := forgeSetupFault(err); cause != "" {
		return &forgeSetupError{cause: forgeHost(f) + ": " + cause, err: err}
	}
	host := forgeHost(f)
	var ae *forgeAPIError
	if errors.As(err, &ae) {
		switch ae.status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return &forgeDownError{what: fmt.Sprintf("%s answered HTTP %d (it is down or overloaded)", host, ae.status), then: "try again in a minute", err: err}
		}
		return err
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return &forgeDownError{what: fmt.Sprintf("%s didn't answer within %d s", host, max(1, int(waited.Round(time.Second)/time.Second))), then: "try again", err: err}
	case errors.As(err, &ne):
		return &forgeDownError{what: fmt.Sprintf("couldn't reach %s (%s)", host, PublicError(err)), then: "try again", err: err}
	}
	return err
}

// forgeHost is the forge's host name, for messages.
func forgeHost(f *Forge) string {
	if f == nil {
		return "the forge"
	}
	if u, err := url.Parse(f.BaseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return cmp(f.Name, "the forge")
}
