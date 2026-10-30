package hubanalytics

import (
	"time"
)

type DiagnosticsCapacity struct {
	Hour  time.Time `json:"hour"`
	Slots float64   `json:"slots"`
	Todo  *int      `json:"todo"`
}
