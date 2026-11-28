package core

import (
	"context"
	"errors"
	"net/netip"
	"strings"

	"blankerr/internal/core/kernel"
)

type Service struct{}

// tx wraps the kernel's transaction, as core's does: Part and Exec are the
// kernel's, promoted.
type tx struct{ *kernel.Tx }

func (s *Service) read() (string, error) { return "", errors.New("down") }
func (s *Service) check() error          { return nil }
func (s *Service) count() int            { return 0 }
func (t *tx) load() error                { return nil }

func other() error { return nil }

func discards(s *Service, t *tx) {
	_, _ = s.read()  // want `the error from read is discarded`
	_ = s.check()    // want `the error from check is discarded`
	_ = t.load()     // want `the error from load is discarded`
	v, _ := s.read() // want `the error from read is discarded`
	_ = v
	_ = s.count() // not an error
	_ = other()   // not a method on Service or tx
	if _, err := s.read(); err != nil {
		return
	}
}

func waived(s *Service) {
	//wplint:ignore blankerr a best-effort cache warm; nothing waits on it
	_ = s.check()
}

func (s *Service) held() bool {
	_, err := s.read()
	return err == nil // want `returns only a bool, decided by testing an error`
}

func (s *Service) mixed() bool {
	_, err := s.read()
	return err != nil || strings.TrimSpace("x") == "" // want `returns only a bool`
}

// An answer and an error: the caller can hold.
func (s *Service) answers() (bool, error) {
	_, err := s.read()
	return err == nil, err
}

// A bool that tests no error.
func (s *Service) plain(n int) bool {
	return n == 0
}

// A nil check of something that isn't an error.
func (s *Service) pointer(p *int) bool {
	return p != nil
}

func (s *Service) closure() bool {
	f := func() bool {
		_, err := s.read()
		return err == nil // want `returns only a bool`
	}
	return f()
}

// An error the function was given to classify is the answer.
func isGone(err error) bool {
	return err != nil && strings.Contains(err.Error(), "gone")
}

// A string that doesn't parse is an answer, not a failed read.
func isLoopback(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func (s *Service) save(v string) error { return nil }
func (s *Service) note(v string)       {}
func (s *Service) both() (int, error)  { return 0, nil }

func dropped(s *Service, t *tx) {
	s.save("a")       // want `the error from save is dropped`
	defer s.save("b") // want `the error from deferred save is dropped`
	go s.save("c")    // want `the error from started with go: save is dropped`
	t.load()          // want `the error from load is dropped`
	s.both()          // want `the error from both is dropped`
	s.note("d")       // no error result
	if err := s.save("e"); err != nil {
		return
	}
	//wplint:ignore blankerr best effort; the next sweep redoes it
	s.save("f")
}

func emptied(s *Service) {
	err := s.save("a")
	if err != nil { // want `tests an error and does nothing with it`
	}
	if err := s.save("b"); err != nil { // want `tests an error and does nothing`
	}
	if err != nil {
		_ = err
	}
	var p *int
	if p != nil { // not an error
	}
}

func promoted(ctx context.Context, t *tx) {
	t.Part(ctx, nil) // want `the error from Part is dropped`
	_ = t.Exec()     // want `the error from Exec is discarded`
}
