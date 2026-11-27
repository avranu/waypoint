// Command coremap maps the references between the file groups of
// internal/core (WP-1664, internal/coremap):
//
//	go run ./cmd/coremap [-dir internal/core] [-groups internal/coremap/groups.txt] [-json]
//
// See internal/coremap's package documentation for how to read the output.
package main

import (
	"fmt"
	"os"

	"git.jmann.me/claude/waypoint/internal/coremap"
)

func main() {
	if err := coremap.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "coremap:", err)
		os.Exit(2)
	}
}
