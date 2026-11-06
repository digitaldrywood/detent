package accountstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReleasePreference struct {
	Version   string
	BuildTime time.Time
}

type DailySummaryPreference struct {
	Configured bool
	Enabled    bool
	SendMinute int
	TimeZone   string
	Weekdays   bool
	WindowEnd  int64
	Recipient  string
	Token      string
}

type Repository interface {
	ReleasePreference(context.Context, tracker.OrganizationID, string, string, string) (ReleasePreference, error)
	SetReleasePreference(context.Context, tracker.OrganizationID, string, string, string, ReleasePreference) error
	DailySummaryPreference(context.Context, tracker.OrganizationID, string, string, string) (DailySummaryPreference, error)
	SetDailySummaryPreference(context.Context, tracker.OrganizationID, string, string, string, DailySummaryPreference, string, int64) error
	UnsubscribeDailySummary(context.Context, tracker.OrganizationID, string) error
}
