//go:build !darwin && !linux

package hostmetrics

import "context"

func readPressure(context.Context, *reading) {}

func readDisk(string) (uint64, uint64, bool) {
	return 0, 0, false
}
