package lazyre

import (
	"sync"
	"testing"
)

func TestRegexpCompilesOnFirstUseAndConcurrently(t *testing.T) {
	re := New(`^(\w+)-(\d+)$`)
	if re.re != nil {
		t.Fatal("compiled before use")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m := re.FindStringSubmatch("WP-12"); len(m) != 3 || m[2] != "12" {
				t.Errorf("match: %v", m)
			}
		}()
	}
	wg.Wait()
	if re.String() != `^(\w+)-(\d+)$` || re.ReplaceAllString("a-1", "$2") != "1" {
		t.Error("String or ReplaceAllString")
	}
}

func TestCompileAllReportsABadPattern(t *testing.T) {
	if err := CompileAll(); err != nil {
		t.Fatalf("clean registry: %v", err)
	}
	New(`(unclosed`)
	if err := CompileAll(); err == nil {
		t.Error("a bad pattern was not reported")
	}
	all.Lock()
	all.list = all.list[:len(all.list)-1]
	all.Unlock()
}
