package core

// judgementsChanged runs refreshMeaning (goal_meaning.go) in the
// background, apart from the trace recompute: model calls can take minutes
// and must not hold up traces or goalsSettle (WP-1035). One pass runs at a
// time; a change while it runs starts one more after it, and so does a
// pass that left a backlog it made headway on. It stops with the service.
func (s *Service) judgementsChanged() {
	s.goals.mu.Lock()
	defer s.goals.mu.Unlock()
	s.judgementsChangedLocked()
}

// judgementsChangedLocked is judgementsChanged with goals.mu held.
func (s *Service) judgementsChangedLocked() {
	if s.goals.judging {
		s.goals.judgeDirty = true
		return
	}
	s.goals.judging = true
	idle := make(chan struct{})
	s.goals.judgeIdle = idle
	s.goBG(func() {
		for {
			more := false
			if s.bgCtx.Err() == nil {
				more = s.refreshMeaning(s.bgCtx)
			}
			s.goals.mu.Lock()
			if more {
				s.goals.judgeDirty = true
			}
			if !s.goals.judgeDirty || s.bgCtx.Err() != nil {
				// Closed with the flag cleared, so a wait never sees one
				// without the other (WP-1372).
				s.goals.judging, s.goals.judgeDirty = false, false
				close(idle)
				s.goals.mu.Unlock()
				return
			}
			s.goals.judgeDirty = false
			s.goals.mu.Unlock()
		}
	})
}
