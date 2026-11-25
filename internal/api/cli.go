package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var cliFileRE = regexp.MustCompile(`^(wp-(linux|darwin|windows)-(amd64|arm64)(\.exe)?|waypoint-sandboxd-linux-(amd64|arm64)|SHA256SUMS)$`)

const installScript = `#!/bin/sh
# Installs wp, the Waypoint command-line client, for {{BASE}}.
# Set WP_INSTALL_DIR to choose where (default ~/.local/bin).
#   curl -fsSL {{BASE}}/cli | sh                 install wp
#   curl -fsSL {{BASE}}/setup | sh               install wp, then set this machine up (wp setup)
#   curl -fsSL {{BASE}}/setup | sh -s -- --yes   the same without questions (wp setup -h for more)
set -eu
base='{{BASE}}'
setup={{SETUP}}
if [ "${1:-}" = setup ]; then setup=1; shift; fi
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "wp: no build for this CPU ($arch)" >&2; exit 1 ;;
esac
case "$os" in
  linux|darwin) ;;
  *) echo "wp: no installer for $os; on Windows download $base/cli/wp-windows-amd64.exe" >&2; exit 1 ;;
esac
file="wp-$os-$arch"
dir="${WP_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/cli/$file" -o "$tmp/$file"
curl -fsSL "$base/cli/SHA256SUMS" -o "$tmp/SHA256SUMS"
want=$(grep " $file\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$file" | cut -d' ' -f1)
else got=$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1); fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then echo "wp: checksum mismatch; not installing" >&2; exit 1; fi
chmod +x "$tmp/$file"
mv "$tmp/$file" "$dir/wp"
"$dir/wp" config server "$base" >/dev/null
echo "Installed wp to $dir/wp, pointed at $base"
case ":$PATH:" in *":$dir:"*) ;; *) echo "Add $dir to your PATH." ;; esac
if [ "$setup" = 1 ]; then
  echo
  rm -rf "$tmp"
  trap - EXIT
  # This script is wp's stdin, so wp setup asks its one question on the
  # terminal (/dev/tty); with no terminal, pass --yes.
  exec "$dir/wp" setup ${1+"$@"}
fi
echo "Next: wp setup (signs in, and sets up the Claude Code hooks and a runner)"
`

func (s *Server) cliInstall(w http.ResponseWriter, r *http.Request) {
	s.writeInstallScript(w, "0")
}

// cliSetup is the installer that goes on to `wp setup`.
func (s *Server) cliSetup(w http.ResponseWriter, r *http.Request) {
	s.writeInstallScript(w, "1")
}

func (s *Server) writeInstallScript(w http.ResponseWriter, setup string) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	script := strings.NewReplacer("{{BASE}}", s.cfg.BaseURL, "{{SETUP}}", setup).Replace(installScript)
	w.Write([]byte(script)) // #nosec G705 -- a fixed script; BaseURL is operator config
}

func (s *Server) cliFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !cliFileRE.MatchString(name) || s.cfg.CLIDir == "" {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.cfg.CLIDir, name) // #nosec G703 -- name matched cliFileRE: no separators
	if _, err := os.Stat(path); err != nil {  // #nosec G703 -- as above
		http.Error(w, "this server was built without the wp CLI binaries", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path) // #nosec G703 -- as above
}
