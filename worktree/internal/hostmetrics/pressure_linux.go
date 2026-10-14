package hostmetrics

import (
	"context"
	"os"
	"strings"

	"github.com/digitaldrywood/detent/internal/hostpressure"
)

func readPressure(ctx context.Context, sample *reading) {
	if ctx.Err() != nil {
		return
	}
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return
	}
	pressure, err := hostpressure.Parse(string(data))
	if err != nil {
		return
	}
	sample.psiSome, sample.psiSomeOK = pressure.Some.Avg10, true
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "full ") {
			sample.psiFull, sample.psiFullOK = pressure.Full.Avg10, true
			break
		}
	}
}
