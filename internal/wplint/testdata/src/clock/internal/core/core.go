package core

import "time"

type Service struct{ clock func() time.Time }

func (s *Service) now() time.Time { return s.clock() }

func work() {}

func (s *Service) Stamp() time.Time {
	return time.Now() // want `use s.now\(\), the clock tests control`
}

func (s *Service) Took() time.Duration {
	start := time.Now()
	work()
	start = time.Now()
	work()
	return time.Since(start)
}

func (s *Service) Inline() time.Duration {
	t := s.now()
	return time.Now().Sub(t) // want `use s.now\(\)`
}

func (s *Service) Since(at time.Time) time.Duration {
	return time.Since(at) // want `time.Since measures against the real clock`
}

func (s *Service) Steps() time.Duration {
	start := time.Now()
	work()
	end := time.Now()
	return end.Sub(start) + time.Now().Sub(start)
}

func (s *Service) Mixed() time.Duration {
	start := time.Now() // want `use s.now\(\)`
	return s.now().Sub(start)
}

func (s *Service) Trailing() (time.Time, time.Time) {
	a := time.Now() //wplint:ignore clock a test of a trailing waiver
	b := time.Now() // want `use s.now\(\)`
	return a, b
}

func (s *Service) Declared() time.Duration {
	var start time.Time
	start = time.Now()
	var other = time.Now()
	a, b := time.Now(), time.Now()
	return time.Since(start) + time.Since(other) + time.Since(a) + time.Until(b)
}

func free() time.Time { return time.Now() }

func (s *Service) Kept() time.Time {
	at := time.Now() // want `use s.now\(\)`
	return at
}

func (s *Service) Waived() time.Time {
	//wplint:ignore clock a test of the waiver
	return time.Now()
}

func (s *Service) NoReason() time.Time {
	/* want `wplint:ignore clock needs a reason` */ //wplint:ignore clock
	return time.Now()                               // want `use s.now\(\)`
}
