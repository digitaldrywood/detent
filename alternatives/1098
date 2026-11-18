package gotestreceipt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func withSocketPath(path string, action func(string) error) (err error) {
	if len(path) <= maxSocketPath {
		return action(path)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	return action(fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(path)))
}
