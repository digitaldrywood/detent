package hubserver

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func validateLocalDatabaseFilesystem(directory string) error {
	volume := filepath.VolumeName(directory)
	if volume == "" {
		return fmt.Errorf("inspect hub database filesystem: no volume for %s", directory)
	}
	if strings.HasPrefix(volume, `\\`) {
		return fmt.Errorf("%w: %s uses a UNC volume", ErrNetworkFilesystem, directory)
	}

	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return fmt.Errorf("inspect hub database filesystem: %w", err)
	}
	return validateWindowsDriveType(directory, windows.GetDriveType(root))
}

func validateWindowsDriveType(directory string, driveType uint32) error {
	switch driveType {
	case windows.DRIVE_REMOTE:
		return fmt.Errorf("%w: %s uses a remote drive", ErrNetworkFilesystem, directory)
	case windows.DRIVE_UNKNOWN, windows.DRIVE_NO_ROOT_DIR:
		return fmt.Errorf("inspect hub database filesystem: drive type %d for %s", driveType, directory)
	default:
		return nil
	}
}
