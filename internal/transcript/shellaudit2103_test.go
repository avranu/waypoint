package transcript

import "testing"

// Audit 2103: $$ before a brace hides nothing after it (.1), a relative write
// in a record with no directory is unknown (.7), and sed's expression may be
// attached to its option (.10).
func TestShellEditAudit2103(t *testing.T) {
	for _, c := range []struct {
		cmd, cwd     string
		wrote, known bool
	}{
		{`echo $${; echo x > a.go`, "/r", true, true},
		{`echo ${ echo x > a.go; }`, "/r", true, true},
		{`echo x > out.txt`, "", false, false},
		{`echo x > /tmp/out.txt`, "", false, true},
		{`ls`, "", false, true},
		{`sed -i --expression='s/a/b/' main.go`, "/r", true, true},
		{`sed -i -e's/a/b/' main.go`, "/r", true, true},
		{`sed -i --file=fix.sed main.go`, "/r", true, true},
		{`sed -ffix.sed -i main.go`, "/r", true, true},
		{`sed -ne 's/a/b/p' -i main.go`, "/r", true, true},
		{`sed -i.bak 's/a/b/' main.go`, "/r", true, true},
		{`sed -i 's/a/b/'`, "/r", false, true},
		{`sed -n -i 's/a/b/' /tmp/x`, "/r", false, true},
	} {
		if wrote, known := (&ShellSession{}).Edit(c.cmd, c.cwd); wrote != c.wrote || known != c.known {
			t.Errorf("Edit(%q, %q) = %v, %v, want %v, %v", c.cmd, c.cwd, wrote, known, c.wrote, c.known)
		}
	}
}
