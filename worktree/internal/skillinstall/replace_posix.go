//go:build !windows

package skillinstall

import "os"

func replaceFile(source string, destination string) error {
	return os.Rename(source, destination)
}

func safeFileMode(mode os.FileMode) bool {
	return mode.Perm() == fileMode
}
