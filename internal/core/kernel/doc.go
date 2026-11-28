// Package kernel is what every domain package of Waypoint's core needs: the
// errors, the actor, users and permissions, and small value helpers now, and
// later transactions, events and clock. It imports no other package under
// internal/core, so a domain package can import it without a cycle. The
// decision to split core into layered packages is WP-ADR151.
package kernel
