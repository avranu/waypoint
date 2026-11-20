package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionValidate(t *testing.T) {
	ok := func() *Session {
		return &Session{ID: "wp-470-1", Class: ClassUnattended, Argv: []string{"/usr/bin/make", "check"},
			Env: map[string]string{"GOFLAGS": "-mod=mod"}, Egress: json.RawMessage(`{"rules":[]}`)}
	}
	if err := ok().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mut  func(*Session)
		want string
	}{
		"id":          {func(s *Session) { s.ID = "Up" }, "session id"},
		"long id":     {func(s *Session) { s.ID = strings.Repeat("a", 41) }, "session id"},
		"class":       {func(s *Session) { s.Class = "root" }, "class"},
		"no argv":     {func(s *Session) { s.Argv = nil }, "0 words"},
		"relative":    {func(s *Session) { s.Argv = []string{"make"} }, "absolute"},
		"unclean":     {func(s *Session) { s.Argv = []string{"/usr/../bin/sh"} }, "absolute"},
		"nul":         {func(s *Session) { s.Argv = []string{"/bin/sh", "a\x00b"} }, "NUL"},
		"home":        {func(s *Session) { s.Env = map[string]string{"HOME": "/root"} }, "sets HOME itself"},
		"lower proxy": {func(s *Session) { s.Env = map[string]string{"http_proxy": "x"} }, "itself"},
		"all proxy":   {func(s *Session) { s.Env = map[string]string{"ALL_PROXY": "x"} }, "itself"},
		"ld":          {func(s *Session) { s.Env = map[string]string{"LD_LIBRARY_PATH": "/x"} }, "itself"},
		"name":        {func(s *Session) { s.Env = map[string]string{"A=B": "x"} }, "variable name"},
		"newline":     {func(s *Session) { s.Env = map[string]string{"A": "x\ny"} }, "newline"},
		"memory":      {func(s *Session) { s.Resources.MemoryMB = MaxResources.MemoryMB + 1 }, "memory_mb"},
		"negative":    {func(s *Session) { s.Resources.CPUs = -1 }, "cpus"},
		"no egress":   {func(s *Session) { s.Egress = nil }, "egress"},
	} {
		s := ok()
		c.mut(s)
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	var none *Session
	if err := none.Validate(); err == nil {
		t.Error("a nil session passed")
	}
	if r := (Resources{CPUs: 2}).WithDefaults(); r.CPUs != 2 || r.MemoryMB != DefaultResources.MemoryMB {
		t.Errorf("defaults %+v", r)
	}
}
