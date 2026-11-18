package procgroup

import (
	"os"

	"github.com/digitaldrywood/detent/internal/procgroup/darwinproc"
)

func scratchLaunchFile(env []string) (*os.File, int, error) {
	file, err := darwinproc.OpenScratch(env)
	return file, darwinproc.ScratchFD - 3, err
}
