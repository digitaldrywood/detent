//go:build !darwin && !linux

package cli

import (
	"errors"
	"io"
)

func copyRunningExecutable(io.Writer, string) error {
	return errors.ErrUnsupported
}
