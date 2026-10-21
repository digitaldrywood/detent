//go:build !windows

package cli

import "os"

func openLogsFile(name string) (*os.File, error) {
	return os.Open(name)
}
