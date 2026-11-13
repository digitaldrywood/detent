package githuboutbound

import (
	"context"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"log/slog"
	"time"
)

type Deps struct {
	Repository              tenantstore.Repository
	Organization            tracker.OrganizationID
	Details                 attachment.DetailStore
	Backend                 githubcontract.OutboxBackend
	Now                     func() time.Time
	Jitter                  func() float64
	NewLeaseID              func() string
	NewTimer                func(time.Duration) Timer
	OutboxPollInterval      time.Duration
	OutboxProcessingTimeout time.Duration
	OutboxBaseBackoff       time.Duration
	OutboxMaxBackoff        time.Duration
	Logger                  *slog.Logger
	Disabled                error
	Unavailable             error
	SplitRepository         func(string) (string, string, bool)
	Process                 func(context.Context) (bool, error)
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
