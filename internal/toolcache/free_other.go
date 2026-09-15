//go:build !darwin && !linux && !windows

package toolcache

import "errors"

func volumeFreeBytes(string) (uint64, error) {
	return 0, errors.New("cache volume free-space inspection unsupported on this platform")
}
