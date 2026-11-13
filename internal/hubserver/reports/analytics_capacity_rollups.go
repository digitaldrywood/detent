package reports

import "time"

type analyticsCapacitySample struct {
	ID        string
	At        time.Time
	AtNS      int64
	StartNS   int64
	Start     time.Time
	SecondsNS int64
	Valid     bool
}
