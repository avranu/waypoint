// Package repocheck holds checks over the repository's own files that run
// with the Go tests: keeping a site's names, as SITE_NAMES lists them, out
// of the tree (WP-459), and how the deploy scripts read their settings. It
// has no code of its own: the checks are its tests.
package repocheck
