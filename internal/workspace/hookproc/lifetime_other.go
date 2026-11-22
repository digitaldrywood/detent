//go:build !unix

package hookproc

import (
	"context"
	"errors"
	"os"
)

func NewLifetime() (*Lifetime, error)       { return nil, errors.ErrUnsupported }
func newLifetime(string) (*Lifetime, error) { return nil, errors.ErrUnsupported }

func openRetainedLifetime(*os.Root, string) (*Lifetime, error) { return nil, errors.ErrUnsupported }

func openRetainedFile(*os.Root, string, os.FileInfo) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

func (*Lifetime) fileIdentity() (string, error) {
	return "", errors.ErrUnsupported
}

func (*Lifetime) Active() (bool, error) { return false, errors.ErrUnsupported }

func (*Lifetime) Capture(context.Context, string, string) (bool, error) {
	return false, errors.ErrUnsupported
}
