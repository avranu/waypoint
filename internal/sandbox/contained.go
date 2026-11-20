package sandbox

import "os"

// Machine users' uids (PL7 §5.1) come from this range; the helper never hands
// one out twice.
const (
	FirstUID = 40000
	LastUID  = 49999
)

// EnvLevel names the variable the helper sets in every session it starts to
// the session's level. A spec can't set it (reservedEnv).
const EnvLevel = "WAYPOINT_SANDBOX"

// InContained reports whether this process runs inside a Contained session:
// the helper set the level, and the process runs as one of its machine
// users. The uid is what makes it hold: the variable alone could be set by
// anyone, but only the helper starts processes as a machine user.
func InContained() bool {
	uid := os.Getuid()
	return os.Getenv(EnvLevel) == LevelContained && uid >= FirstUID && uid <= LastUID
}
