package reports

import "time"

type analyticsSkipSample struct {
	Source, Reason string
	At             time.Time
	AtNS           int64
}
