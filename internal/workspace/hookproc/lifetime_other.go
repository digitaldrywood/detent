//go:build !unix

package hookproc

import (
	"context"
	"errors"
)

func NewLifetime() (*Lifetime, error)   { return nil, errors.ErrUnsupported }
func (*Lifetime) Active() (bool, error) { return false, errors.ErrUnsupported }

func (*Lifetime) Capture(context.Context, string, string) (bool, error) {
	return false, errors.ErrUnsupported
}
