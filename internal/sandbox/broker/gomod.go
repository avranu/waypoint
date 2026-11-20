package broker

import (
	"strings"
)

// The Go template limits the module proxy to the modules a repo's go.mod
// needs, plus the toolchain (PL7 §7.1): a session can't use the proxy to
// fetch, or to leak data through the name of, any other module path.

// ToolchainModule is what `go` fetches to switch toolchains.
const ToolchainModule = "golang.org/toolchain"

// goProxyCheck reads a GOPROXY protocol path: /<module>/@v/list,
// /<module>/@v/<version>.(info|mod|zip), /<module>/@latest, and the
// checksum database proxied under /sumdb/sum.golang.org/.
func goProxyCheck(modules []string, method, path string) decision {
	if method != "GET" && method != "HEAD" {
		return deny("method")
	}
	if !cleanPath(path) {
		return deny("path")
	}
	if rest, ok := strings.CutPrefix(path, "/sumdb/sum.golang.org"); ok {
		if rest == "/supported" {
			return allow()
		}
		return goSumCheck(modules, method, rest)
	}
	var esc string
	switch {
	case strings.HasSuffix(path, "/@latest"):
		esc = strings.TrimSuffix(path, "/@latest")
	case strings.Contains(path, "/@v/"):
		i := strings.Index(path, "/@v/")
		esc = path[:i]
		if !goProxyFile(path[i+len("/@v/"):]) {
			return deny("path")
		}
	default:
		return deny("path")
	}
	return goModuleAllowed(modules, strings.TrimPrefix(esc, "/"))
}

// goProxyFile is the part after /@v/: "list" or <version>.info|.mod|.zip.
func goProxyFile(f string) bool {
	if f == "list" {
		return true
	}
	if strings.Contains(f, "/") {
		return false
	}
	for _, ext := range []string{".info", ".mod", ".zip"} {
		if v, ok := strings.CutSuffix(f, ext); ok && v != "" {
			return true
		}
	}
	return false
}

// goSumCheck reads checksum database paths: /latest, /tile/... (which name
// no module), and /lookup/<module>@<version>.
func goSumCheck(modules []string, method, path string) decision {
	if method != "GET" && method != "HEAD" {
		return deny("method")
	}
	if !cleanPath(path) {
		return deny("path")
	}
	switch {
	case path == "/latest", strings.HasPrefix(path, "/tile/"):
		return allow()
	}
	rest, ok := strings.CutPrefix(path, "/lookup/")
	if !ok {
		return deny("path")
	}
	at := strings.LastIndex(rest, "@")
	if at <= 0 {
		return deny("path")
	}
	return goModuleAllowed(modules, rest[:at])
}

func goModuleAllowed(modules []string, escaped string) decision {
	mod, ok := unescapeModule(escaped)
	if !ok {
		return deny("path")
	}
	if mod == ToolchainModule {
		return allow()
	}
	for _, m := range modules {
		if mod == m || strings.HasPrefix(mod, m+"/") {
			return allow()
		}
	}
	return deny("module")
}

// unescapeModule undoes the proxy's case encoding: "!x" is "X", and an
// upper-case letter is never valid as it stands.
func unescapeModule(esc string) (string, bool) {
	var b strings.Builder
	bang := false
	for _, c := range esc {
		switch {
		case bang:
			if c < 'a' || c > 'z' {
				return "", false
			}
			b.WriteRune(c - 'a' + 'A')
			bang = false
		case c == '!':
			bang = true
		case c >= 'A' && c <= 'Z':
			return "", false
		default:
			b.WriteRune(c)
		}
	}
	if bang {
		return "", false
	}
	mod := b.String()
	return mod, validModule(mod)
}

// validModule is a loose module path check: enough to refuse anything
// that isn't one, not Go's full rules.
func validModule(m string) bool {
	if m == "" || strings.HasPrefix(m, "/") || strings.HasSuffix(m, "/") || strings.Contains(m, "//") {
		return false
	}
	for _, seg := range strings.Split(m, "/") {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, "@") {
			return false
		}
	}
	for _, c := range m {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("-._~/+", c)
		if !ok {
			return false
		}
	}
	first, _, _ := strings.Cut(m, "/")
	return strings.Contains(first, ".")
}
