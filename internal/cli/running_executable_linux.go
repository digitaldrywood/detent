package cli

import (
	"errors"
	"io"
	"os"
)

func copyRunningExecutable(dst io.Writer, _ string) error {
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, file)
	return errors.Join(err, file.Close())
}
