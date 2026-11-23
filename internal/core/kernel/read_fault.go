package kernel

import (
	"context"
	"database/sql"
	"errors"
)

// ReadFault says whether err is a read that went wrong, the kind worth a
// warning: not nil, and not the caller giving up (a cancelled context is
// never a fault). Not-found, forbidden and unsupported are answers, which
// each site splits off before it asks.
func ReadFault(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled)
}

// MissingOrDenied says whether err is an answer about an item: it isn't
// there, or the actor may not see it. Those are the normal "no" of a read
// made for someone, not a fault.
func MissingOrDenied(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthorized) || errors.Is(err, sql.ErrNoRows)
}
