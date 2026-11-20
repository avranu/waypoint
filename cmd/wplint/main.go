// Command wplint runs Waypoint's own static checks (internal/wplint).
//
//	wplint ./cmd/... ./internal/...      the Go rules (a go/analysis multichecker)
//	wplint repo [-base origin/main]      the rules about files: migrations, e2e waits
package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/tools/go/analysis/multichecker"

	"git.jmann.me/claude/waypoint/internal/wplint"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "repo" {
		os.Exit(repo(os.Args[2:]))
	}
	multichecker.Main(wplint.Analyzers...)
}

func repo(args []string) int {
	fs := flag.NewFlagSet("wplint repo", flag.ExitOnError)
	var o wplint.RepoOptions
	fs.StringVar(&o.Root, "root", ".", "repository root")
	fs.StringVar(&o.Base, "base", "origin/main", "the ref merged migrations and e2e waits are compared with")
	fs.BoolVar(&o.RequireBase, "require-base", false, "fail when the base ref is missing (CI)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	findings, err := wplint.CheckRepo(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wplint:", err)
		return 2
	}
	for _, f := range findings {
		fmt.Fprintln(os.Stderr, f)
	}
	if len(findings) > 0 {
		return 1
	}
	return 0
}
