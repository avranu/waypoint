package kernel

type Actor struct{ ID int64 }

// The kernel has a Service of its own here; the rule leaves the kernel alone.
type Service struct{ Open int }

func (s *Service) Close() {}
