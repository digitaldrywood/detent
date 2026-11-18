package hookproc

import (
	"os"
	"strconv"

	"github.com/digitaldrywood/detent/internal/procgroup/darwinproc"
)

func exactAttemptScratch(path string) bool { return darwinproc.AttemptScratch(path) }
func attemptScratchFD() string             { return strconv.Itoa(darwinproc.ScratchFD) }
func holdsAttemptScratch(pid int, dir os.FileInfo) (bool, error) {
	return darwinproc.HoldsScratch(pid, dir)
}
