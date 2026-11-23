// Package sub is a package under internal/core: the rule follows code there.
package sub

import (
	"context"
	"log/slog"
)

func probe(ctx context.Context, l *slog.Logger) {
	l.Warn("probe failed") // want `use WarnContext\(ctx, …\)`
	l.WarnContext(ctx, "probe failed")
}
