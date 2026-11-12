package credits

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Plan struct {
	Name            string
	AllowanceMicros *int64
}

type Pack struct {
	PriceID  string `json:"price_id" yaml:"price_id"`
	Label    string `json:"label" yaml:"label"`
	USDCents int64  `json:"usd_cents" yaml:"usd_cents"`
}

type Deps struct {
	Funding *Funding
	NewID   func() string
	Binding func(context.Context) (billing.Binding, error)
	Unbound error

	ReadIncluded   bool
	Repository     tenantstore.Repository
	Organization   tracker.OrganizationID
	Mode           string
	Now            func() time.Time
	PaymentNow     func() time.Time
	CostMultiplier float64
	Packs          []Pack
	Plan           func(context.Context, billingstore.Reader, billingstore.PlanReference) (Plan, error)
	ReadReason     func(context.Context, billingstore.AuditReference) (string, error)
	ArchiveReason  func(context.Context, string) (billingstore.AuditReference, error)
	SafeID         func(string) bool
	Details        attachment.DetailStore
	ValidateDetail func(context.Context, attachment.DetailStore, tracker.OrganizationID, attachment.ArchiveReference, []byte) error
}

type Service struct{ deps Deps }

func New(deps Deps) *Service {
	if deps.PaymentNow == nil {
		deps.PaymentNow = deps.Now
	}
	return &Service{deps: deps}
}

func (s *Service) read(ctx context.Context, run func(tenantstore.Reader) error) error {
	if s.deps.Repository == nil {
		return errors.New("billing repository is unavailable")
	}
	return s.deps.Repository.ReadTenant(ctx, s.deps.Organization, corestore.Serving, run)
}

func (s *Service) write(ctx context.Context, run func(tenantstore.Writer) error) error {
	if s.deps.Repository == nil {
		return errors.New("billing repository is unavailable")
	}
	return s.deps.Repository.WriteTenant(ctx, s.deps.Organization, corestore.Serving, run)
}

type Usage struct {
	OccurredAt     time.Time
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ConversationID string
	TurnID         string
	Provider       string
	Model          string
	Tokens         runner.AgentTokenCounts
	Outcome        conversation.Delivery
}

type Window struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Hourly bool      `json:"-"`
}

type Summary struct {
	Range           Window  `json:"range"`
	Input           int64   `json:"input"`
	CachedInput     int64   `json:"cached_input"`
	Output          int64   `json:"output"`
	ReasoningOutput int64   `json:"reasoning_output"`
	Tokens          int64   `json:"tokens"`
	Turns           int64   `json:"turns"`
	UnpricedTurns   int64   `json:"unpriced_turns"`
	CostUSD         float64 `json:"cost_usd"`
}

func UsageCost(price billingstore.ConversationPrice, tokens runner.AgentTokenCounts) float64 {
	return (float64(tokens.InputTokens-tokens.CachedInputTokens)*price.Input + float64(tokens.CachedInputTokens)*price.CachedInput + float64(tokens.OutputTokens)*price.Output) / 1e6
}
