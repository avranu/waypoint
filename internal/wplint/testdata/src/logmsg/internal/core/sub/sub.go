// Package sub is a package under internal/core: the rule follows code there.
package sub

import (
	"context"
	"fmt"
	"log/slog"
)

func probe(ctx context.Context, l *slog.Logger, n int) {
	l.WarnContext(ctx, fmt.Sprintf("probe %d failed", n)) // want `a warning's message is fixed text`
	l.WarnContext(ctx, "probe failed", "n", n)
}
