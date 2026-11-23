package stray

// A domain the order doesn't list may import only the kernel.
import (
	_ "corelayers/internal/core/forge" // want `stray is not in wplint's coreOrder`
	_ "corelayers/internal/core/kernel"
)
