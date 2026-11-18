//go:build !unix && !windows

package toolcache

import (
	"errors"
	"os"
)

func tryCacheLock(*os.File, bool) error {
	return errors.New("managed Go build cache locking is unavailable")
}

func cacheEntryPinned(*os.Root, string) (bool, error) {
	return true, nil
}
