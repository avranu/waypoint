package pr

// A test file is held to the same rule.
import _ "corelayers/internal/core" // want `a package under internal/core must not import internal/core`
