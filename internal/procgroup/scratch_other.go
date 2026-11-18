//go:build !darwin && !windows

package procgroup

import "os"

func scratchLaunchFile([]string) (*os.File, int, error) { return nil, 0, nil }
