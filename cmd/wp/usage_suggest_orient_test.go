package main

import "testing"

func TestBashWritesEndOrientation(t *testing.T) {
	for cmd, want := range map[string]bool{
		"sed -i 's/a/b/' x.go":        true,
		"sed -n '1,5p' x.go":          false,
		"perl -pi -e 's/a/b/' x.go":   true,
		"echo hi | tee out.txt":       true,
		"git apply fix.patch":         true,
		"grep -rn foo . | head":       false,
		"sed --in-place s/a/b/ x.go":  true,
		"cd d && sed -i.bak s/a/b/ f": true,
	} {
		if got := editCall(sugCall{Tool: "Bash", Command: cmd}); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

func TestBashWritesOptionParsing(t *testing.T) {
	for cmd, want := range map[string]bool{
		"perl -Ilib -e 'print 1'":     false,
		"perl -pi -e 's/a/b/' f":      true,
		"perl -i.bak -pe 's/a/b/' f":  true,
		"sed -n -e p f":               false,
		"sed -i s/a/b/ f":             true,
		"sed -ni p f":                 true,
		"git -C repo apply fix.patch": true,
		"git apply --check fix.patch": false,
		"git apply --check --apply p": true,
		"git -c a=b status":           false,
	} {
		if got := bashWrites(cmd); got != want {
			t.Errorf("bashWrites(%q) = %v, want %v", cmd, got, want)
		}
	}
}
