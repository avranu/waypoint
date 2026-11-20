package clean

import "os"

func Token() string { return os.Getenv("WAYPOINT_TOKEN") }
