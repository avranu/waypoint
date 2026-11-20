//go:build !unix

package core

import "errors"

func diskSpace(dir string) (free, total int64, err error) {
	return 0, 0, errors.New("disk space isn't read on this system")
}
