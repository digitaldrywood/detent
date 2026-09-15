package toolcache

import (
	"os"
	"path/filepath"
)

// FreeBytes returns space available to this user on the cache volume.
// A not-yet-created cache uses its nearest existing ancestor's volume.
func FreeBytes(root string) (uint64, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	for {
		_, err := os.Stat(root)
		if err == nil {
			return volumeFreeBytes(root)
		}
		if !os.IsNotExist(err) {
			return 0, err
		}
		parent := filepath.Dir(root)
		if parent == root {
			return 0, err
		}
		root = parent
	}
}
