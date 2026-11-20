package main

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// What counts as a read from one of the repo's environments (WP-749,
// WP-ADR90). The guard lists what leaves a request a read, rather than what
// makes it a change: it can't keep up with every way a tool sends a body or
// another method (abbreviated long options, config files, curl --next and
// --expand-*, wget -e, other protocols), so anything it doesn't know is
// refused there.

// readMethods are the HTTP methods that only read.
var readMethods = map[string]bool{"GET": true, "HEAD": true, "OPTIONS": true}

// readSpec lists the options of a fetch tool that leave a request a read.
type readSpec struct {
	short    string   // short options without a value
	shortVal string   // short options with one
	long     []string // long options without a value
	longVal  []string // long options with one
	method   []string // options whose value is the request method
	header   []string // options whose value is a header
	format   []string // options whose value is an output format (curl -w)
}

var (
	curlRead = readSpec{
		short:    "sSfiIvkL46NgG",
		shortVal: "oHmwAXreDu",
		long: []string{"--silent", "--show-error", "--fail", "--fail-with-body", "--include", "--head", "--verbose", "--insecure", "--location",
			"--compressed", "--no-buffer", "--http1.1", "--http2", "--globoff", "--get", "--no-progress-meter", "--ipv4", "--ipv6"},
		longVal: []string{"--output", "--header", "--max-time", "--connect-timeout", "--write-out", "--user-agent", "--retry", "--retry-delay",
			"--retry-max-time", "--request", "--url", "--oauth2-bearer", "--dump-header", "--referer", "--user", "--range", "--max-filesize",
			"--cacert", "--max-redirs", "--url-query"},
		method: []string{"-X", "--request"},
		header: []string{"-H", "--header"},
		format: []string{"-w", "--write-out"},
	}
	// curl -G: data goes into the query (curl 8 keeps -G for the whole
	// call, and --next, which would end it, isn't a read option).
	curlGetRead = readSpec{
		short: curlRead.short, shortVal: curlRead.shortVal + "d",
		long:    curlRead.long,
		longVal: append(slices.Clip(curlRead.longVal), "--data", "--data-urlencode", "--data-raw", "--data-ascii"),
		method:  curlRead.method, header: curlRead.header, format: curlRead.format,
	}
	wgetRead = readSpec{
		short:    "qSvNnc",
		shortVal: "OoTtU",
		long: []string{"--quiet", "--verbose", "--no-verbose", "--server-response", "--spider", "--no-check-certificate", "--content-on-error",
			"--timestamping", "--continue", "--no-clobber"},
		longVal: []string{"--output-document", "--output-file", "--timeout", "--tries", "--header", "--user-agent", "--method", "--max-redirect",
			"--user", "--password", "--http-user", "--http-password"},
		method: []string{"--method"},
		header: []string{"--header"},
	}
	httpieRead = readSpec{
		short:    "vhbjIqFS",
		shortVal: "aAopsP",
		long: []string{"--verbose", "--headers", "--body", "--json", "--ignore-stdin", "--quiet", "--follow", "--stream",
			"--check-status", "--all"},
		longVal: []string{"--auth", "--auth-type", "--output", "--print", "--pretty", "--style", "--timeout", "--verify", "--max-redirects",
			"--history-print"},
	}
)

// methodHeader: a header that asks the server to treat the request as
// another method (X-HTTP-Method-Override and its kin), or headers read from
// a file the guard can't see.
func methodHeader(h string) bool {
	name, _, _ := strings.Cut(h, ":")
	return strings.HasPrefix(h, "@") || strings.Contains(strings.ToLower(name), "method")
}

// writeOutFormat: a curl --write-out format that writes a file (%output{},
// curl 8.3 and later) or is read from one, where it could (@file).
func writeOutFormat(v string) bool {
	return strings.HasPrefix(v, "@") || strings.Contains(strings.ToLower(v), "%output")
}

// scanRead checks args against spec: the positionals, and the first
// argument that makes the call more than a read ("" when none does).
func scanRead(args []string, spec readSpec) (pos []string, seen map[string]bool, bad string) {
	seen = map[string]bool{}
	value := func(flag, v, arg string) string {
		switch {
		case slices.Contains(spec.method, flag) && !readMethods[v]: // curl sends -X get as get
			return flag + " " + v
		case slices.Contains(spec.header, flag) && methodHeader(v):
			return arg
		case slices.Contains(spec.format, flag) && writeOutFormat(v):
			return arg
		}
		return ""
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(pos, args[i+1:]...), seen, ""
		case strings.HasPrefix(a, "--"):
			name, v, has := strings.Cut(a, "=")
			seen[name] = true
			switch {
			case slices.Contains(spec.long, name) && !has:
			case slices.Contains(spec.longVal, name):
				if !has {
					if i+1 >= len(args) {
						return pos, seen, a
					}
					i++
					v = args[i]
				}
				if b := value(name, v, a); b != "" {
					return pos, seen, b
				}
			default:
				return pos, seen, a
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
		short:
			for j := 1; j < len(a); j++ {
				seen["-"+string(a[j])] = true
				if strings.IndexByte(spec.short, a[j]) >= 0 {
					continue
				}
				if strings.IndexByte(spec.shortVal, a[j]) < 0 {
					return pos, seen, a
				}
				f, v := "-"+string(a[j]), a[j+1:]
				if v == "" {
					if i+1 >= len(args) {
						return pos, seen, a
					}
					i++
					v = args[i]
				}
				if b := value(f, v, a); b != "" {
					return pos, seen, b
				}
				break short
			}
		default:
			pos = append(pos, a)
		}
	}
	return pos, seen, ""
}

// envChange is what makes a call to one of the repo's environments more
// than a read ("" for a plain read): any option outside the tool's
// readSpec, a method other than GET, HEAD or OPTIONS, or, for httpie and xh,
// a data item or no --ignore-stdin (from an agent's shell, whose standard
// input isn't a terminal, they send that input as a body: a POST without a
// method, and a GET with a body when something is piped in).
func envChange(name string, args []string) string {
	switch name {
	case "curl":
		_, seen, bad := scanRead(args, curlRead)
		if bad != "" && (seen["-G"] || seen["--get"]) {
			// With -G, curl puts the data in the query of a GET.
			if _, seen, b := scanRead(args, curlGetRead); b == "" && (seen["-G"] || seen["--get"]) {
				return ""
			}
		}
		return bad
	case "wget":
		_, seen, bad := scanRead(args, wgetRead)
		if bad == "" && !seen["-O"] && !seen["--output-document"] && !seen["--spider"] {
			// Without one, wget saves into the working directory under a
			// name from the URL, over any file of that name.
			return "no -O, so wget writes a file in the working directory"
		}
		return bad
	case "http", "https", "xh":
		pos, seen, bad := scanRead(args, httpieRead)
		if bad != "" {
			return bad
		}
		// seen holds options, not their values: -oIx is -o with the value Ix.
		stdin := seen["--ignore-stdin"] || seen["-I"]
		if len(pos) > 1 && httpMethod(pos[0]) {
			if !readMethods[strings.ToUpper(pos[0])] {
				return pos[0]
			}
			pos = pos[1:]
		}
		if !stdin {
			return "no --ignore-stdin, so standard input is sent as a body"
		}
		for _, item := range pos[min(1, len(pos)):] {
			i := strings.IndexAny(item, ":=@")
			if i < 0 || strings.Contains(item, `\`) { // a\==b is the data field a= (httpie escapes)
				return item
			}
			next := byte(0)
			if i+1 < len(item) {
				next = item[i+1]
			}
			switch {
			case item[i] == '=' && next == '=': // query
			case item[i] == ':' && next != '=' && !methodHeader(item): // header
			default:
				return item
			}
		}
		return ""
	}
	return name + " (use curl)" // aria2c, fetch
}

// fetchConfig is a settings file the tool reads on its own, which could
// add a body, a method or more addresses the guard can't see; "" when there
// is none. The variables that move them are refused (fetchSettingsVar).
func fetchConfig(name string) string {
	var files []string
	// HOME, and the account's home, which the tools fall back on when a
	// command unsets HOME.
	for _, home := range []string{os.Getenv("HOME"), passwdHome()} {
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" && home != "" {
			xdg = filepath.Join(home, ".config")
		}
		switch name {
		case "curl":
			files = append(files, filepath.Join(os.Getenv("CURL_HOME"), ".curlrc"), filepath.Join(xdg, ".curlrc"), filepath.Join(xdg, "curlrc"),
				filepath.Join(home, ".curlrc"), filepath.Join(home, "_curlrc"))
		case "wget":
			files = append(files, os.Getenv("WGETRC"), filepath.Join(home, ".wgetrc"), filepath.Join(xdg, "wget", "wgetrc"))
		case "http", "https":
			files = append(files, filepath.Join(os.Getenv("HTTPIE_CONFIG_DIR"), "config.json"), filepath.Join(xdg, "httpie", "config.json"),
				filepath.Join(home, ".httpie", "config.json"))
		case "xh":
			files = append(files, filepath.Join(os.Getenv("XH_CONFIG_DIR"), "config.json"), filepath.Join(xdg, "xh", "config.json"))
		}
	}
	for _, f := range files {
		if f == "" || !filepath.IsAbs(f) {
			continue
		}
		if _, err := os.Stat(f); err == nil { // #nosec G703 -- only whether the tool's own settings file exists
			return f
		}
	}
	return ""
}

// httpMethod: an httpie/xh first positional that names a method rather than
// an address (httpie takes any case).
func httpMethod(s string) bool { return httpMethodRE.MatchString(s) }

var httpMethodRE = lazyre.New(`^[A-Za-z]+$`)

// envScheme: u reaches an environment over HTTP(S). curl and wget pick other
// protocols (gopher, dict, telnet, sftp, a guessed ftp) from the address, so
// they must say http:// or https://; httpie and xh speak only HTTP.
func envScheme(name, u string) bool {
	scheme, _, ok := strings.Cut(u, "://")
	if !ok {
		return name == "http" || name == "https" || name == "xh"
	}
	s := strings.ToLower(scheme)
	return s == "http" || s == "https"
}

// passwdHome is the account's home directory from the password database,
// which curl and wget use when HOME is unset.
var passwdHome = func() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}
