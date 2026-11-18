package runnerenroll

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository       tenantstore.Repository
	Now              func() time.Time
	NewID            func(string) string
	GenerateToken    func() (string, error)
	TokenFingerprint func(string) string
	WorkerScope      string
	ApplyUsage       func(context.Context, billingstore.Writer, time.Time, string, bool, bool) error
	CheckAdmission   func(context.Context, billingstore.Writer, time.Time, int64, ...string) error
	ValidPlatform    func(string, string) bool
	ValidSpriteSlug  func(string) bool
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) Redeem(ctx context.Context, organization tracker.OrganizationID, token string, request runnerauth.Redemption) (runnerauth.Identity, error) {
	if !request.Valid() || !runnerauth.ValidCredential(request.Credential) || token == request.Credential || strings.TrimSpace(request.Hostname) == "" || len(request.Hostname) > 200 || len(request.DisplayName) > 200 || request.Capacity < 0 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || !s.deps.ValidPlatform(request.OS, request.Architecture) {
		return runnerauth.Identity{}, hubapi.PublicInvalid("Host identity, a separate generated credential, hostname, version and nonnegative capacity are required")
	}
	if request.SpriteName != "" && (request.SpriteName != request.Hostname || !s.deps.ValidSpriteSlug(request.SpriteName)) {
		return runnerauth.Identity{}, hubapi.PublicInvalid("Sprite name must match the runner hostname")
	}
	if err := request.BackendIsolation.Validate(); err != nil {
		return runnerauth.Identity{}, hubapi.Invalid(err.Error())
	}
	if request.IsolationTier == "" {
		request.IsolationTier = isolation.Sandbox
	}
	if request.IsolationTier != isolation.Sandbox && request.IsolationTier != isolation.NativeTrusted {
		return runnerauth.Identity{}, hubapi.PublicInvalid("Isolation tier must be sandbox or native-trusted")
	}
	return s.RedeemTenant(ctx, organization, token, request)
}
