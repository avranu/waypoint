package kernel

import (
	_ "corelayers/internal/core/forge" // want `internal/core/kernel must not import corelayers/internal/core/forge`
	_ "errors"
)
