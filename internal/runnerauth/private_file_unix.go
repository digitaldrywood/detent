//go:build !windows

package runnerauth

import (
	"errors"
	"os"
)

func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func createPrivateTemp(directory, prefix string) (*os.File, error) {
	return os.CreateTemp(directory, prefix+"*")
}

func openPrivateFile(path string, _ bool) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("runner credential file is unavailable")
	}
	return file, nil
}
