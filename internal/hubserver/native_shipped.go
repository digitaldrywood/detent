package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var nativeShippedWindowSpans = []struct {
	name string
	span time.Duration
}{{"1h", time.Hour}, {nativeDefaultShippedWindow, 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}}

const nativeDefaultShippedWindow = "24h"

// nativeShippedWindow names the board counter window. Windows the board no
// longer offers fall back to the default so saved views keep working.
func nativeShippedWindow(value string) (string, error) {
	switch value {
	case "1h", "7d":
		return value, nil
	case "", nativeDefaultShippedWindow, "48h", "14d", "all":
		return nativeDefaultShippedWindow, nil
	default:
		return "", nativePublicInvalid("completed_window supports 1h,24h,7d")
	}
}

func applyNativeShippedWindows(summary *tracker.NativeWorkSummary, windows []tracker.NativeShippedWindow, selected string) {
	summary.ShippedWindows = windows
	for _, window := range windows {
		if window.Window == selected {
			summary.Completed, summary.Cancelled = window.Shipped, window.Cancelled
		}
	}
}
