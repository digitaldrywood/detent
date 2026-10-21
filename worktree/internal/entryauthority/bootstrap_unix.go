//go:build !windows

package entryauthority

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func inheritedChannel(budget time.Duration) (channel, error) {
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	for _, fd := range []int{3, 4} {
		if err := syscall.SetNonblock(fd, true); err != nil {
			return channel{}, errors.Join(err, syscall.Close(3), syscall.Close(4))
		}
	}
	stream := channel{reader: os.NewFile(3, "entry-authority-read"), writer: os.NewFile(4, "entry-authority-write"), budget: budget}
	for _, file := range []*os.File{stream.reader, stream.writer} {
		info, err := file.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return channel{}, errors.Join(cloudassert.ErrInvalid, stream.Close())
		}
	}
	return stream, nil
}
