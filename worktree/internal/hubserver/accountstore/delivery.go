package accountstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type SummaryRecipient struct {
	User, Organization, Email, Principal, Role, Token, TimeZone string
	Eligible, Weekdays                                          bool
	Minute                                                      int
	End                                                         time.Time
}

type SummaryReceipt struct {
	User        string
	End         time.Time
	Status      string
	RecordedAt  time.Time
	NextEnd     time.Time
	NextDue     time.Time
	RetryJitter time.Duration
}

type SummaryDeliveryRepository interface {
	DueSummaries(context.Context, tracker.OrganizationID, time.Time) ([]SummaryRecipient, error)
	SummaryStatus(context.Context, tracker.OrganizationID, string, time.Time) (string, error)
	ClaimSummary(context.Context, tracker.OrganizationID, SummaryReceipt) (bool, error)
	FinishSummary(context.Context, tracker.OrganizationID, SummaryReceipt) error
	AdvanceSummary(context.Context, tracker.OrganizationID, SummaryReceipt) error
}
