//go:build !darwin && !linux

package procgroup

import (
	"os"
	"time"
)

func UsageFromState(*os.ProcessState, time.Duration) *Usage {
	return nil
}
