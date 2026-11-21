// Package repocheck holds checks over the repository's own files that run
// with the Go tests: keeping a site's names out of the tree (WP-459; CI
// reads the SITE_NAMES secret, locally SITE_NAMES in the environment or
// deploy/site.mk wins, else the per-user site-names file every checkout and
// worktree shares, WP-1432), and how the deploy scripts read their settings. It
// has no code of its own: the checks are its tests.
package repocheck
