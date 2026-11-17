package credits

import (
	"context"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type FundingInput struct {
	Enabled   bool   `json:"enabled"`
	Threshold int64  `json:"threshold_cents"`
	Price     string `json:"price"`
}

type checkoutKey struct {
	IdempotencyKey string `json:"idempotency_key"`
}

type HTTPDeps struct {
	Service              func() *Service
	PlatformService      func() *Service
	Auth                 func() *hubauth.Authenticator
	Error                func(echo.Context, error) error
	InvalidRequest       func(echo.Context, error) error
	CommandFailure       func(echo.Context, bool, error) error
	Failure              func(echo.Context, bool, int, string) error
	Destination          func(echo.Context, bool, string) error
	Return               func() string
	ValidateKey          func(string) error
	Checkout             func(context.Context, func(context.Context) (hubauth.Credential, error), string, string) (string, error)
	ConfigureFunding     func(context.Context, func(context.Context) (hubauth.Credential, error), FundingInput, string) (FundingInput, error)
	NewID                func() string
	Audit                func(context.Context, hubauth.Credential, echo.Context) error
	SharedClaims         func(echo.Context) (cloudassert.Claims, bool)
	JSONError            func(echo.Context, int, string) error
	InternalError        func(echo.Context, string, string, error) error
	PlatformOrganization func() tracker.OrganizationID
}

type HTTPService struct{ deps HTTPDeps }

func NewHTTP(deps HTTPDeps) *HTTPService { return &HTTPService{deps: deps} }

func RegisterWriteRoutes(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	e.POST("/organization/billing/credits/checkout", s.hostedCreditCheckout)
	e.POST("/organization/billing/credits/auto-fund", s.hostedCreditAutoFund)
	e.POST("/api/v2/organizations/:organization/billing/credits/checkout", s.hostedCreditCheckout)
	e.PUT("/api/v2/organizations/:organization/billing/credits/auto-fund", s.hostedCreditAutoFund)
}
func RegisterReadRoutes(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	e.GET("/api/v2/organizations/:organization/billing/credits/transactions", s.hostedCreditTransactions)
}
func RegisterPlatformRoutes(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	e.POST("/internal/v1/platform/ai-credits", s.AdjustPlatform)
	e.POST("/internal/v1/platform/ai-credits/balance", s.PlatformBalance)
}
func (s *HTTPService) owner(ctx context.Context, c echo.Context) (hubauth.Credential, error) {
	credential, _, err := s.deps.Auth().HostedCredential(ctx, c)
	if err != nil || credential.Hosted == nil || credential.HostedRole != "owner" || credential.Hosted.SupportActor != "" {
		return hubauth.Credential{}, auth.ErrHostedIdentity
	}
	return credential, nil
}

func (s *HTTPService) hostedCreditCheckout(c echo.Context) error {
	api := strings.HasPrefix(c.Path(), "/api/v2/")
	if _, err := s.owner(c.Request().Context(), c); err != nil {
		return s.deps.CommandFailure(c, api, err)
	}
	var request struct {
		checkoutKey
		Price string `json:"price"`
	}
	if api {
		if err := hubapi.DecodeJSON(c, &request); err != nil {
			return s.deps.InvalidRequest(c, err)
		}
		if err := s.deps.ValidateKey(request.IdempotencyKey); err != nil {
			return s.deps.Error(c, err)
		}
	} else {
		request.Price = c.FormValue("price")
	}
	authorize := func(ctx context.Context) (hubauth.Credential, error) { return s.owner(ctx, c) }
	result, err := s.deps.Checkout(c.Request().Context(), authorize, request.Price, request.IdempotencyKey)
	if err != nil {
		return s.deps.CommandFailure(c, api, err)
	}
	return s.deps.Destination(c, api, result)
}

func (s *HTTPService) hostedCreditAutoFund(c echo.Context) error {
	api := strings.HasPrefix(c.Path(), "/api/v2/")
	if _, err := s.owner(c.Request().Context(), c); err != nil {
		return s.deps.CommandFailure(c, api, err)
	}
	var request FundingInput
	if api {
		if err := hubapi.DecodeJSON(c, &request); err != nil {
			return s.deps.InvalidRequest(c, err)
		}
	} else {
		request.Enabled = c.FormValue("enabled") == "on"
		request.Price = c.FormValue("price")
		threshold, err := strconv.ParseInt(c.FormValue("threshold_cents"), 10, 64)
		if err != nil && request.Enabled {
			return s.deps.Failure(c, api, http.StatusBadRequest, "Enter a threshold in USD cents")
		}
		request.Threshold = threshold
	}
	authorize := func(ctx context.Context) (hubauth.Credential, error) { return s.owner(ctx, c) }
	if _, err := s.deps.ConfigureFunding(c.Request().Context(), authorize, request, ""); err != nil {
		return s.deps.CommandFailure(c, api, err)
	}
	if api {
		return c.NoContent(http.StatusNoContent)
	}
	return c.Redirect(http.StatusSeeOther, s.deps.Return())
}

func creditPageQuery(c echo.Context) (Page, error) {
	p := Page{Cursor: c.QueryParam("cursor"), From: c.QueryParam("from"), To: c.QueryParam("to")}
	if value := c.QueryParam("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return p, hubapi.Invalid("credit transaction limit must be between 1 and 100")
		}
		p.Limit = n
	}
	return p, nil
}

func (s *HTTPService) hostedCreditTransactions(c echo.Context) error {
	credential, err := s.owner(c.Request().Context(), c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	p, err := creditPageQuery(c)
	if err != nil {
		return s.deps.Error(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	if c.QueryParam("format") == "csv" {
		return s.exportCreditTransactions(ctx, c, credential, p)
	}
	view, err := s.deps.Service().Read(ctx, p)
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

func (s *HTTPService) exportCreditTransactions(ctx context.Context, c echo.Context, credential hubauth.Credential, p Page) error {
	from, to, err := p.Bounds()
	if err != nil {
		return s.deps.Error(c, err)
	}

	return s.exportPostgresCreditTransactions(ctx, c, credential, from, to)
}

func (s *HTTPService) creditCSVWriter(ctx context.Context, c echo.Context, credential hubauth.Credential) (*csv.Writer, error) {
	auditCtx := mutation.WithContext(ctx, mutation.Metadata{Action: "billing_export", Source: "http", CorrelationID: s.deps.NewID()})
	if err := s.deps.Audit(auditCtx, credential, c); err != nil {
		return nil, s.deps.Error(c, err)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/csv; charset=utf-8")
	c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="credit-transactions.csv"`)
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	writer := csv.NewWriter(c.Response())
	if err := writer.Write([]string{"recorded_at", "kind", "turn_id", "tokens", "charged_amount_usd", "included_amount_usd"}); err != nil {
		return nil, err
	}
	return writer, nil
}

func (s *HTTPService) exportPostgresCreditTransactions(ctx context.Context, c echo.Context, credential hubauth.Credential, from, to int64) error {
	started, err := s.deps.Service().Export(ctx, from, to, func() (*csv.Writer, error) { return s.creditCSVWriter(ctx, c, credential) })
	if !started && err != nil {
		return s.deps.Error(c, err)
	}
	return err
}
func (s *HTTPService) PlatformBalance(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	claims, ok := s.deps.SharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.deps.JSONError(c, http.StatusForbidden, "Shared service access is required to read AI credits")
	}
	result, err := s.deps.PlatformService().PlatformBalance(ctx)
	if err != nil {
		return s.deps.InternalError(c, "ai_credits_unavailable", "AI credits are unavailable", err)
	}
	return c.JSON(http.StatusOK, result)
}
func (s *HTTPService) AdjustPlatform(c echo.Context) error {
	claims, ok := s.deps.SharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Subject == "" || claims.Email == "" {
		return s.deps.JSONError(c, http.StatusForbidden, "Shared service identity and email are required to adjust AI credits")
	}
	var adjustment billing.CreditAdjustment
	if err := hubapi.DecodeJSON(c, &adjustment); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	balance, err := s.deps.PlatformService().Adjust(c.Request().Context(), claims.Email, adjustment)
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"organization_id": s.deps.PlatformOrganization(), "balance_micros": balance})
}
