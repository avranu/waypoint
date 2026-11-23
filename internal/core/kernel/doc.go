// Package kernel is what every domain package of Waypoint's core needs: the
// errors now, and later the actor, transactions, events and clock. It
// imports no other package under internal/core, so a domain package can
// import it without a cycle. The decision to split core into layered
// packages is recorded for WP-1499.
package kernel
