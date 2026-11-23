// Package sub is a package under internal/core: the rule follows code there.
package sub

import "errors"

type Service struct{}

func (s *Service) read() (string, error) { return "", errors.New("down") }

func discards(s *Service) {
	_, _ = s.read() // want `the error from read is discarded`
}

func (s *Service) held() bool {
	_, err := s.read()
	return err == nil // want `returns only a bool, decided by testing an error`
}
