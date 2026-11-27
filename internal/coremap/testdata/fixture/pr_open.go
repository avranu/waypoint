package fixture

// PR is a pull request.
type PR struct{ Number int }

// OpenPR opens one.
func (s *Service) OpenPR() *PR {
	s.tick()
	s.db++
	return ptr(PR{Number: 1})
}

// afterOpen reacts to an opened pull request by calling up into issues.
func afterOpen(s *Service) {
	s.CloseIssue()
	notify(1)
	_ = notify // upward, but not a call
}
