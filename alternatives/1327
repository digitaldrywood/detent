package hostmetrics

import (
	"context"

	"golang.org/x/sys/unix"
)

func readPressure(ctx context.Context, sample *reading) {
	if ctx.Err() != nil {
		return
	}
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err == nil && (level == 1 || level == 2 || level == 4) {
		sample.pressureLevel, sample.pressureOK = level, true
	}
}
