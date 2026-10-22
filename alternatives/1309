//go:build !unix

package gobudget

import "errors"

func tryLock(string) (func() error, error) {
	return nil, errors.ErrUnsupported
}
