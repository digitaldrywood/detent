//go:build windows

package socket

import "errors"

func Verify(string) error {
	return errors.New("shared entry tenant sockets are not supported on Windows")
}
