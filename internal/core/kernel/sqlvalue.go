package kernel

import "time"

// NullInt is v for a SQL argument, or nil (NULL) when v is nil.
func NullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// MillisToTime is a unix-millisecond column value as a UTC time.
func MillisToTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// BoolInt is b as the 0 or 1 a SQL boolean column holds.
func BoolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
