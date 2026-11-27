// Package coremap maps the references between groups of files in one Go
// package, to pick what to move out of internal/core next and to measure what
// is left (WP-1664, WP-ADR151). The logic is here; cmd/coremap is the command.
//
// Run `make coremap`, which waits for the machine's tests slot (`go run
// ./cmd/coremap` does not). The package is type-checked once with go/types, as
// built for linux/amd64 without cgo on any host. Every identifier use in file A
// that resolves to a package-level object, method or struct field declared in
// file B is one reference from group(A) to group(B); references within a group
// are ignored. Each is classed type, field, func, service-method (a method on
// Service), method or var (variables and constants), and flagged when it is a
// call.
//
// Groups come from groups.txt in this directory. Its line order is the
// provider-first layer order, bottom first, taken from -suggest-order (its
// header says how it is kept). A reference to a group earlier in the order
// points "down"; one to a later group points "up" and has to be cut (an event,
// a moved helper, a small interface) before the lower group can leave the
// package. A group not listed in groups.txt is "unplaced": it sits at the top
// of the order and is named in the summary. A file no line matches falls to the
// group named by its stem before the first underscore; when that group is
// listed, kernel included, the summary names the file (a test keeps
// internal/core's own files all matched by a line). The group "kernel" is package core's root files, not the
// package internal/core/kernel.
//
// The default report has three parts. The summary gives file and line counts
// (the package, then each subpackage; test files apart), the fields and methods
// of Service, the strongly connected components of the group graph (each of two
// or more groups is listed as a cycle; one big one means the layers are
// tangled), and how many references point down and up, and how many upward ones
// are calls. A second pair of figures leaves out references that touch the
// kernel group. The heaviest upward edges follow (-top N, default 5), each with
// its most used symbols. The group table ends the report: files, lines,
// references in and out, upward references out, the distinct symbols those
// upward references name (what has to be cut), references to the kernel group,
// and "ready", meaning no references up or to the kernel group. References to
// the kernel group count as down, but what they name (Service among it) is
// declared in package core, which a package below it cannot import, so a group
// with any cannot leave core as it is. Ready only means something once every
// file is placed. Output order is fixed so two runs diff. A package with type
// errors gives an incomplete map, and the command then exits with an error.
//
// Queries print instead of the report (with -json, one at a time):
//
//	-edge 'A>B'          every reference from group A to group B, file:line, symbol
//	-group G             what G needs from other groups, and what others need from G
//	-kernel-candidates   symbols used from at least -min-groups (10) other groups
//	-affinity            each group's size and the three groups it exchanges the most
//	                     references with, both directions, with their share of its total
//	-write-upward        write the group pairs that refer upward to upward.txt (make
//	                     coremap-upward), naming those not listed before; not with
//	                     -suggest-order or -per-file, nor with type errors; with
//	                     another -dir or -groups, only to another -upward
//
// Quote -edge's value, a bare > being a shell redirect: through make that is
// CORE_ARGS="-edge 'pr>issues'".
//
// Two flags change how groups are formed or ordered, for the report and the
// queries alike:
//
//	-suggest-order       order the groups to reduce upward references (kernel pinned
//	                     first; Eades-Lin-Smyth, then single-node moves: a heuristic)
//	                     and report under that order
//	-per-file            every file is its own group, for file-level -affinity or
//	                     -group; always in the suggested order, groups.txt ordering
//	                     no files (its kernel files stay first)
//
// The suggested order depends on the groups and on the references in the code.
// To bring groups.txt up to date, paste its "order:" line's order into the
// file's line order.
//
// -group answers "what would a narrow interface need": the symbols under "G
// needs" marked UP are the upward ones. -json prints the same data as JSON.
// Timings go to stderr.
//
// Cost: the first step, "go list -export -deps", compiles every dependency the
// build cache lacks, so a run on a cold cache is far slower than a later one.
package coremap
