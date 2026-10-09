//go:build darwin || linux

package procgroup

import (
	"os"
	"runtime"
	"syscall"
	"time"
)

func UsageFromState(state *os.ProcessState, wall time.Duration) *Usage {
	if state == nil {
		return nil
	}
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage == nil {
		return nil
	}
	user := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
	system := time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
	return NormalizeUsage(runtime.GOOS, usage.Maxrss, user, system, wall)
}
