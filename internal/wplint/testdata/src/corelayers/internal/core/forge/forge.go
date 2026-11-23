package forge

// top is above forge in the order: no cycle, so only the lint refuses it.
import _ "corelayers/internal/core/top" // want `forge must not import top, which is not below it`
