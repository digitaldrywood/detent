package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOperatorAuthorizationFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"denied", accessstore.ErrDenied, 403, "access_denied"},
		{"invalid assertion", cloudassert.ErrInvalid, 403, "access_denied"},
		{"revoked identity", auth.ErrHostedIdentity, 403, "access_denied"},
		{"missing credential", corestore.ErrNotFound, 403, "access_denied"},
		{"missing row", sql.ErrNoRows, 403, "access_denied"},
		{"wrapped denial", fmt.Errorf("check: %w", operatortool.ErrAccessDenied), 403, "access_denied"},
		{"provider", &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}, 503, "provider_unavailable"},
		{"cancelled", context.Canceled, 503, "service_unavailable"},
		{"deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), 503, "service_unavailable"},
		{"nested authorization", &operatortool.AuthorizationError{Stage: "check", Cause: context.DeadlineExceeded}, 503, "service_unavailable"},
		{"serialization", corestore.ErrSerializationFailure, 503, "service_unavailable"},
		{"pool", &pgconn.PgError{Code: "53300", Message: "customer-secret"}, 503, "service_unavailable"},
		{"connection", &pgconn.PgError{Code: "08006", Message: "customer-secret"}, 503, "service_unavailable"},
		{"closed connection", sql.ErrConnDone, 503, "service_unavailable"},
		{"bad connection", driver.ErrBadConn, 503, "service_unavailable"},
		{"unknown", errors.New("customer-secret"), 503, "service_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &Service{config: Config{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}}
			response := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response)
			err := operatorAuthorizationError(test.err)
			if err == nil {
				t.Fatal("authorization error became success")
			}
			if err := s.nativeAPIError(c, err); err != nil {
				t.Fatal(err)
			}
			var failure apiErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status || failure.Code != test.code {
				t.Errorf("status=%d code=%s, want %d %s", response.Code, failure.Code, test.status, test.code)
			}
			if test.status == 403 && failure.Message != operatortool.ErrAccessDenied.Error() {
				t.Errorf("denial message=%q", failure.Message)
			}
			if strings.Count(logs.String(), "\n") != 1 || !strings.Contains(logs.String(), `"error":`) {
				t.Errorf("expected one edge error log: %s", logs.String())
			}
			if strings.Contains(logs.String()+response.Body.String(), "customer-secret") {
				t.Fatal("customer content escaped error boundary")
			}
		})
	}
}

func TestOperatorAuthorizationPostgresCancellation(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "owner", Name: "Example", Hash: string(org), Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "owner", MembershipID: "member", Role: "owner", Active: true, PrincipalID: "owner", CreatedAt: now, UpdatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"mcp/healthy", "mcp/cancelled", "mcp/deadline", "board/healthy", "board/cancelled", "board/deadline"} {
			t.Run(string(org)+"/"+mode, func(t *testing.T) {
				transport, mode, _ := strings.Cut(mode, "/")
				var logs bytes.Buffer
				s := &Service{database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), now: func() time.Time { return now }, Hosted: &HostedConfig{OrganizationID: string(org), SharedEntry: &HostedSharedEntry{Issuer: "entry", Generation: 1}, PublicURL: "https://example.test"}}}
				ctx := t.Context()
				if mode == "cancelled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if mode == "deadline" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, now.Add(-time.Second))
					defer cancel()
				}
				response := httptest.NewRecorder()
				c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"name":"work_list"}`)).WithContext(ctx), response)
				c.SetParamNames("organization")
				c.SetParamValues(string(org))
				c.Set(hostedSharedClaimsKey, cloudassert.Claims{Audience: string(org), ProviderOrganization: "provider_" + string(org), Subject: "owner", Role: "owner", Kind: cloudassert.KindKey, IssuedAt: now, ExpiresAt: now.Add(time.Hour), Key: &apikey.KeyAuthority{ID: "key", Kind: "personal", Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "all"}}})
				reached := false
				middleware := s.operatorAuthority
				if transport == "board" {
					middleware = s.entryKeyReadAuthority
				}
				if err := middleware(func(c echo.Context) error { reached = true; return c.NoContent(http.StatusNoContent) })(c); err != nil {
					t.Fatal(err)
				}
				if mode == "healthy" {
					if !reached || response.Code != 204 {
						t.Fatalf("healthy authorization: reached=%t status=%d body=%s", reached, response.Code, response.Body.String())
					}
					return
				}
				var failure apiErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
					t.Fatal(err)
				}
				if reached || response.Code != 503 || failure.Code != "service_unavailable" {
					t.Fatalf("authorization reached=%t status=%d body=%s", reached, response.Code, response.Body.String())
				}
				if strings.Count(logs.String(), "\n") != 1 {
					t.Fatalf("edge logs=%s", logs.String())
				}
			})
		}
	}
}
