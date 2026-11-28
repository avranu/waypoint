package fixture

// Issue links a pull request.
type Issue struct{ PR *PR }

func notify(n int) {}

// CloseIssue closes one.
func (s *Service) CloseIssue() {
	s.tick()
	_ = Issue{}
}

func link(s *Service) *Issue { return &Issue{PR: s.OpenPR()} }
