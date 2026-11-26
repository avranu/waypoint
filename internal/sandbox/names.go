package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// MaxUserName is Linux's limit on a user name.
const MaxUserName = 32

// RepoUser is the user a repo's sandboxes run as.
func RepoUser(slug, id string, salt int) string {
	org, repo, _ := strings.Cut(slug, "/")
	o, r := clean(org), clean(repo)
	room := MaxUserName - len("wpr-") - len("-") - 6 - len("-")
	if len(o)+len(r) > room {
		o = cut(o, max(6, room-len(r)))
		r = cut(r, room-len(o))
	}
	return join("wpr", o, r, hash6(id, salt))
}

// OrgUser is the user a forge owner's multi-repo sessions run as.
func OrgUser(owner, id string, salt int) string {
	room := MaxUserName - len("wpo-") - len("-") - 6
	return join("wpo", cut(clean(owner), room), hash6(id, salt))
}

func hash6(id string, salt int) string {
	if salt > 0 {
		id += "#" + strconv.Itoa(salt)
	}
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])[:6]
}

// clean keeps lower-case letters and digits, with single dashes between
// runs of anything else.
func clean(s string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(s) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(c)
			dash = false
		} else {
			dash = true
		}
	}
	return b.String()
}

func cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) > n {
		s = s[:n]
	}
	return strings.Trim(s, "-")
}

func join(parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "-")
}
