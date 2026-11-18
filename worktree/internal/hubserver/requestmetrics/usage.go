package requestmetrics

import (
	"time"

	"github.com/labstack/echo/v4"
)

type Usage struct {
	Requests, Duration, ResponseBytes, RequestBytes, KnownRequests int64
}

const UsageAmountLimit = int64(1) << 50

func saturatingUsageAmount(a, b int64) int64 {
	return min(max(a, 0), UsageAmountLimit) + min(max(b, 0), UsageAmountLimit)
}

func (u *Usage) Merge(other Usage) {
	u.Requests = min(saturatingUsageAmount(u.Requests, other.Requests), UsageAmountLimit)
	u.Duration = min(saturatingUsageAmount(u.Duration, other.Duration), UsageAmountLimit)
	u.ResponseBytes = min(saturatingUsageAmount(u.ResponseBytes, other.ResponseBytes), UsageAmountLimit)
	u.RequestBytes = min(saturatingUsageAmount(u.RequestBytes, other.RequestBytes), UsageAmountLimit)
	u.KnownRequests = min(saturatingUsageAmount(u.KnownRequests, other.KnownRequests), UsageAmountLimit)
}

func UsageFor(c echo.Context, elapsed time.Duration) Usage {
	usage := Usage{Requests: 1, Duration: min(max(int64(0), elapsed.Microseconds()), UsageAmountLimit), ResponseBytes: min(max(int64(0), c.Response().Size), UsageAmountLimit)}
	if c.Request().ContentLength >= 0 {
		usage.RequestBytes, usage.KnownRequests = min(c.Request().ContentLength, UsageAmountLimit), 1
	}
	return usage
}

func UsageWindows(plans *BillingPlan, now time.Time, pending map[Key]Latency) map[int64]Usage {
	windows := make(map[int64]Usage)
	cutoff := now.Unix()/plans.WindowSeconds*plans.WindowSeconds - plans.RetentionWindows*plans.WindowSeconds
	for key, value := range pending {
		window := key.Minute / plans.WindowSeconds * plans.WindowSeconds
		if window <= cutoff || value.HTTP.Requests == 0 {
			delete(pending, key)
			continue
		}
		usage := windows[window]
		usage.Merge(value.HTTP)
		windows[window] = usage
	}
	return windows
}
