package ranged

import "os"

func Server() string { return os.Getenv("WAYPOINT_SERVER") } // want `add a TestMain that clears WAYPOINT_SERVER and WAYPOINT_TOKEN`
