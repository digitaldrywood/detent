package hostedplans

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type Hosted struct {
	Billing           *HostedBillingConfig
	Plans             *HostedPlansConfig
	PlanID            string
	StorageQuotaBytes int64
	EventQuota        int64
}

type Deps struct {
	Repository     tenantstore.Repository
	Details        attachment.DetailStore
	Organization   tracker.OrganizationID
	Runtime        *atomic.Pointer[RuntimeConfig]
	Loads          *singleflight.Group
	RuntimeBilling *HostedBillingConfig
	HostedBilling  bool
	Logger         *slog.Logger
	Now            func() time.Time
	Configured     func(mode string, multiplier float64, credits bool)
	CurrentTime    func() (time.Time, error)
}

type Service struct {
	billingRepository  tenantstore.Repository
	billingAuditStore  attachment.DetailStore
	hostedOrganization tracker.OrganizationID
	hostedRuntime      *atomic.Pointer[RuntimeConfig]
	runtimeLoads       *singleflight.Group
	runtimeBilling     *HostedBillingConfig
	hostedBilling      bool
	logger             *slog.Logger
	now                func() time.Time
	configured         func(string, float64, bool)
	currentTime        func() (time.Time, error)
}

func New(deps Deps) *Service {
	s := &Service{billingRepository: deps.Repository, billingAuditStore: deps.Details, hostedOrganization: deps.Organization, hostedRuntime: deps.Runtime, runtimeLoads: deps.Loads, runtimeBilling: deps.RuntimeBilling, hostedBilling: deps.HostedBilling, logger: deps.Logger, now: deps.Now, configured: deps.Configured, currentTime: deps.CurrentTime}
	if s.hostedRuntime == nil {
		s.hostedRuntime = new(atomic.Pointer[RuntimeConfig])
	}
	if s.runtimeLoads == nil {
		s.runtimeLoads = new(singleflight.Group)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.configured == nil {
		s.configured = func(string, float64, bool) {}
	}
	if s.currentTime == nil {
		s.currentTime = func() (time.Time, error) { return s.now().UTC(), nil }
	}
	return s
}

func SafeID(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) == -1
}

func parseTimeValue(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}
