package hubserver

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestValidateWindowsDriveType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		driveType   uint32
		wantError   bool
		wantNetwork bool
	}{
		{name: "fixed", driveType: windows.DRIVE_FIXED},
		{name: "remote", driveType: windows.DRIVE_REMOTE, wantError: true, wantNetwork: true},
		{name: "unknown", driveType: windows.DRIVE_UNKNOWN, wantError: true},
		{name: "missing root", driveType: windows.DRIVE_NO_ROOT_DIR, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateWindowsDriveType(`C:\hub`, test.driveType)
			if (err != nil) != test.wantError {
				t.Fatalf("validateWindowsDriveType() error = %v, wantError %t", err, test.wantError)
			}
			if got := errors.Is(err, ErrNetworkFilesystem); got != test.wantNetwork {
				t.Fatalf("validateWindowsDriveType() network error = %t, want %t", got, test.wantNetwork)
			}
		})
	}
}
