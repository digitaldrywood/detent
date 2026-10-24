//go:build unix

package cloudentry

import (
	"os"
	"syscall"
)

func configurationSignals() []os.Signal { return []os.Signal{syscall.SIGHUP} }
