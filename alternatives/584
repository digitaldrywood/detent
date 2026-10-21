package cli

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func createCodexProfileJunction(source string, destination string) error {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	target := `\??\` + strings.TrimPrefix(absolute, `\\?\`)
	if strings.HasPrefix(absolute, `\\`) && !strings.HasPrefix(absolute, `\\?\`) {
		target = `\??\UNC\` + strings.TrimPrefix(absolute, `\\`)
	}
	encoded, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	data := make([]byte, 16+len(encoded)*2+2)
	if len(data) > windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE {
		return fmt.Errorf("codex junction target is too long: %s", source)
	}
	binary.LittleEndian.PutUint32(data[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(data[4:6], uint16(len(data)-8))
	binary.LittleEndian.PutUint16(data[10:12], uint16((len(encoded)-1)*2))
	binary.LittleEndian.PutUint16(data[12:14], uint16(len(encoded)*2))
	for index, value := range encoded {
		binary.LittleEndian.PutUint16(data[16+index*2:], value)
	}
	path, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return fmt.Errorf("create Codex junction directory: %w", err)
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return errors.Join(fmt.Errorf("open Codex junction directory: %w", err), os.Remove(destination))
	}
	var returned uint32
	setErr := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &data[0], uint32(len(data)), nil, 0, &returned, nil)
	closeErr := windows.CloseHandle(handle)
	if setErr != nil {
		return errors.Join(fmt.Errorf("set Codex junction target: %w", setErr), closeErr, os.Remove(destination))
	}
	return closeErr
}
