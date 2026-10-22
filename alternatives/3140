package toolcache

import (
	"errors"
	"os"
	"path/filepath"
)

// FreeBytes returns space available to this user on the cache volume.
// A not-yet-created cache uses its nearest existing ancestor's volume.
func FreeBytes(root string) (uint64, error) {
	return volumeBytes(root, volumeFreeBytes)
}

// CapacityBytes returns total capacity of the cache volume, including for an uncreated cache.
func CapacityBytes(root string) (uint64, error) {
	return volumeBytes(root, volumeCapacityBytes)
}

func volumeBytes(root string, inspect func(string) (uint64, error)) (uint64, error) {
	if root == "" || root == "off" {
		return 0, errors.New("cache volume unavailable")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	for {
		_, err := os.Stat(root)
		if err == nil {
			return inspect(root)
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
