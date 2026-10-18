//go:build windows

package skillinstall

import (
	"os"

	"golang.org/x/sys/windows"
)

func replaceFile(source string, destination string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePointer, destinationPointer, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func safeFileMode(os.FileMode) bool {
	return true
}
