package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
		return "", hubapi.NativePublicInvalid("completed_window supports 1h,24h,7d")
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
