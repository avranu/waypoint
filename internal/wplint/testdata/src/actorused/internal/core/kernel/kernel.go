package kernel

type Actor struct{}

type Service struct{}

func (s *Service) Get(actor *Actor) error { return nil } // want `Get takes an \*Actor and never reads it`
