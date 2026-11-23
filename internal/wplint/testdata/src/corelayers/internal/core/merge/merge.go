package merge

import (
	_ "corelayers/internal/core" // want `a package under internal/core must not import internal/core`
	_ "corelayers/internal/core/kernel"
)
