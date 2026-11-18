//go:build !unix

package hookproc

import "errors"

func NewLifetime() (*Lifetime, error)   { return nil, errors.ErrUnsupported }
func (*Lifetime) Active() (bool, error) { return false, errors.ErrUnsupported }
