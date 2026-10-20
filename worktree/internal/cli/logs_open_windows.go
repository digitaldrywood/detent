//go:build windows

package cli

import (
	"errors"
	"os"
	"syscall"
)

func openLogsFile(name string) (*os.File, error) {
	path, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	handle, err := syscall.CreateFile(
		path,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		if closeErr := syscall.CloseHandle(handle); closeErr != nil {
			return nil, errors.Join(errors.New("create runtime log file handle"), closeErr)
		}
		return nil, errors.New("create runtime log file handle")
	}
	return file, nil
}
