// Package fixture is a small package for coremap's tests: a kernel file, two
// groups that reference each other (a cycle with an upward call) and a file
// the grouping leaves unplaced.
package fixture

// Service holds the state.
type Service struct {
	db    int
	clock int
}

func ptr[T any](v T) *T { return &v }

func (s *Service) tick() { s.clock++ }
