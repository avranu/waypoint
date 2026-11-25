// Package coremap maps the references between groups of files in one Go
// package, to pick what to move out of internal/core next and to measure what
// is left (WP-1664, WP-ADR151). The logic is here; cmd/coremap is the command.
//
// Run `make coremap` (or `go run ./cmd/coremap`). The package is type-checked
// once with go/types, which takes a while for internal/core. Every identifier
// use in file A that resolves to a package-level object, method or struct field
// declared in file B is one reference from group(A) to group(B); references
// within a group are ignored. Each is classed type, field, func,
// service-method (a method on Service), method or var (variables and
// constants), and flagged when it is a call.
//
// Groups come from groups.txt in this directory. Its line order is the
// provider-first layer order, bottom first. A reference to a group earlier in
// the order points "down" and is fine; one to a later group points "up" and has
// to be cut (an event, a moved helper, a small interface) before the lower
// group can leave the package. A group not listed in groups.txt is "unplaced":
// it sits at the top of the order and is named in the summary, a prompt to
// place it in groups.txt.
//
// The default report has three parts. The summary gives file and line counts
// (the package, then each subpackage; test files apart), the fields and methods
// of Service, the strongly connected components of the group graph (one big
// component means the layers are tangled), and how many references point down
// and up, and how many upward ones are calls. A second pair of figures leaves
// out references that touch the kernel group. The heaviest upward edges
// follow (-top N, default 5), each with its most used symbols. The group table
// ends the report: files, lines, references in and out, upward references out,
// and "ready", meaning no upward references out, so the group could move below
// everything above it as it is. Output order is fixed so two runs diff.
//
// Queries print instead of the report:
//
//	-edge A>B            every reference from group A to group B, file:line, symbol
//	-group G             what G needs from other groups, and what others need from G
//	-kernel-candidates   symbols used from at least -min-groups (10) other groups
//
// -group answers "what would a narrow interface need": the symbols under "G
// needs" marked UP are the upward ones. -json prints the same data as JSON.
// Timings go to stderr.
package coremap
