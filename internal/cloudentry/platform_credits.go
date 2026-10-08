package cloudentry

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type platformCredits struct{ service *Service }

type platformCreditFailure struct {
	status int
	body   json.RawMessage
}

func (e *platformCreditFailure) Error() string { return "The tenant rejected the credit adjustment" }

func (s *Service) registerPlatformCredits(ctx context.Context) {
	handler := mcp.NewHTTPHandler(platformCredits{s}, "", mcp.HTTPConfig{
		Logger: s.config.Logger,
		Principal: func(r *http.Request) operatortool.Identity {
			return operatortool.ConnectionIdentity(r.Context())
		},
	})
	s.closePlatformMCP = func() error {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return handler.Shutdown(shutdown)
	}
	s.echo.Any("/api/cloud/platform/mcp", echo.WrapHandler(handler), s.platformCreditAuthority)
	s.echo.POST("/api/cloud/platform/organizations/:organization/ai-credits", s.adjustPlatformAICredits, s.platformCreditAuthority)
}

func (s *Service) platformCreditAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		ctx := c.Request().Context()
		var identity operatortool.Identity
		if header := c.Request().Header.Get(echo.HeaderAuthorization); header != "" {
			scheme, token, ok := strings.Cut(header, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || s.config.Allocation == nil || len(s.config.Allocation.EntitlementAdminToken) < 32 || subtle.ConstantTimeCompare([]byte(apikey.HashToken(token)), []byte(apikey.HashToken(string(s.config.Allocation.EntitlementAdminToken)))) != 1 {
				return c.NoContent(http.StatusForbidden)
			}
			identity = operatortool.Identity{PrincipalID: "entitlement-administrator", OrganizationID: "platform", CredentialID: apikey.HashToken(token)}
		} else {
			session, err := s.session(c)
			if err != nil {
				return c.NoContent(http.StatusUnauthorized)
			}
			if !s.entitlementAdministrator(ctx, session) || !s.csrfValid(c, session, "") {
				return c.NoContent(http.StatusForbidden)
			}
			identity = operatortool.Identity{PrincipalID: session.Subject, OrganizationID: "account:" + session.Subject, CredentialID: session.Hash, SessionID: session.Hash}
		}
		c.SetRequest(c.Request().WithContext(operatortool.WithConnection(ctx, operatortool.Connection{Identity: identity})))
		return next(c)
	}
}

func (a platformCredits) actor(ctx context.Context) (string, error) {
	s := a.service
	id := operatortool.ConnectionIdentity(ctx)
	if id.SessionID == "" {
		if id.PrincipalID != "entitlement-administrator" || id.OrganizationID != "platform" || s.config.Allocation == nil || len(s.config.Allocation.EntitlementAdminToken) < 32 || subtle.ConstantTimeCompare([]byte(id.CredentialID), []byte(apikey.HashToken(string(s.config.Allocation.EntitlementAdminToken)))) != 1 {
			return "", operatortool.ErrAccessDenied
		}
		return id.PrincipalID, nil
	}
	session, err := s.currentAdministrationSession(ctx, id)
	if err != nil || !s.entitlementAdministrator(ctx, session) {
		return "", operatortool.ErrAccessDenied
	}
	return session.Email, nil
}

func (a platformCredits) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := a.actor(ctx); err != nil {
		return nil, err
	}
	return operatortool.PlatformCreditCatalog(), nil
}

func (a platformCredits) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if call.Name != operatortool.PlatformAdjustAICredits {
		return operatortool.Result{}, operatortool.ErrUnknownTool
	}
	var input operatortool.PlatformCreditArguments
	if err := operatortool.DecodeArguments(call.Arguments, &input); err != nil {
		return operatortool.Result{}, err
	}
	return a.adjust(ctx, input)
}

func (a platformCredits) adjust(ctx context.Context, input operatortool.PlatformCreditArguments) (operatortool.Result, error) {
	actor, err := a.actor(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	if !safeID(input.OrganizationID) || !safeID(input.IdempotencyKey) {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	organization, err := a.service.readyOrganization(ctx, input.OrganizationID)
	if err != nil {
		return operatortool.Result{}, err
	}
	status, body, err := a.service.serviceCall(ctx, organization, "/internal/v1/platform/ai-credits", input.CreditAdjustment, func(claims *cloudassert.Claims) {
		claims.Subject, claims.Email = actor, actor
	})
	if err != nil {
		return operatortool.Result{}, err
	}
	if status != http.StatusOK {
		return operatortool.Result{}, &platformCreditFailure{status: status, body: body}
	}
	return operatortool.Result{Content: body}, nil
}

func (s *Service) adjustPlatformAICredits(c echo.Context) error {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 16<<10))
	var adjustment billing.CreditAdjustment
	if err != nil || operatortool.DecodeArguments(raw, &adjustment) != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "The credit adjustment could not be read"})
	}
	result, err := (platformCredits{s}).adjust(c.Request().Context(), operatortool.PlatformCreditArguments{OrganizationID: c.Param("organization"), CreditAdjustment: adjustment})
	if err != nil {
		var failure *platformCreditFailure
		switch {
		case errors.Is(err, operatortool.ErrAccessDenied):
			return c.NoContent(http.StatusForbidden)
		case errors.Is(err, operatortool.ErrInvalidArguments):
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "invalid_request", "message": "Organization and idempotency key are required"})
		case errors.Is(err, ErrOrganizationNotFound):
			return c.NoContent(http.StatusNotFound)
		case errors.As(err, &failure) && (failure.status == http.StatusUnprocessableEntity || failure.status == http.StatusConflict):
			return c.JSONBlob(failure.status, failure.body)
		default:
			return c.JSON(http.StatusBadGateway, map[string]string{"code": "tenant_unavailable", "message": "The organization's Hub is temporarily unavailable"})
		}
	}
	return c.JSONBlob(http.StatusOK, result.Content)
}
