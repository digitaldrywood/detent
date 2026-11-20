package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func testHostSummary(now time.Time) hostmetrics.Summary {
	return hubtest.HostSummary(now)
}
