package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/credits"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDecodeAPIJSONCompatibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		scope   hubapi.APIScope
		body    string
		wantErr bool
		project string
		path    string
		field   string
	}{
		{name: "worker future heartbeat field", scope: hubapi.APIScopeWorker, body: `{"version":"develop-040e7195c","future_observation":true}`},
		{name: "worker newer project configuration", scope: hubapi.APIScopeWorker, body: `{"version":"develop-040e7195c","project_configuration":{"project_id":"project","policy_mismatch":false}}`, project: "project"},
		{name: "operator unknown field", scope: hubapi.APIScopeOperator, body: `{"runner_version":"test"}`, wantErr: true, field: "runner_version"},
		{name: "admin nested unknown field", scope: hubapi.APIScopeAdmin, body: `{"project_configuration":{"project_name":"project"}}`, wantErr: true, field: "project_name"},
		{name: "policy extra retry key", scope: hubapi.APIScopeAdmin, path: hubapi.NativeBase + "/onboarding/policy", body: `{"idempotency_key":"private value"}`, wantErr: true, field: "idempotency_key"},
		{name: "unauthenticated unknown field", body: `{"future_observation":true}`, wantErr: true},
		{name: "worker invalid known field", scope: hubapi.APIScopeWorker, body: `{"version":42}`, wantErr: true, field: "version"},
		{name: "worker unknown run event field", scope: hubapi.APIScopeWorker, path: hubapi.NativeBase + "/work-items/:item/events", body: `{"version":"develop-040e7195c","prompt":"private prompt"}`, wantErr: true},
		{name: "worker multiple values", scope: hubapi.APIScopeWorker, body: `{"version":"test"} {}`, wantErr: true},
		{name: "worker malformed JSON", scope: hubapi.APIScopeWorker, body: `{"version":`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body)), recorder)
			path := test.path
			if path == "" {
				path = hubapi.NativeBase + "/machines/:machine/heartbeat"
			}
			c.SetPath(path)
			if test.scope != "" {
				c.Set("hub_api_credential", hubapi.APICredential{Scope: test.scope})
			}
			var heartbeat struct {
				Version              string `json:"version"`
				ProjectConfiguration struct {
					ProjectID string `json:"project_id"`
				} `json:"project_configuration"`
			}
			err := hubapi.DecodeAPIJSON(c, &heartbeat)
			if (err != nil) != test.wantErr {
				t.Fatalf("decode error = %v, want error %t", err, test.wantErr)
			}
			if test.field != "" {
				if err := (&Service{config: Config{Logger: discardLogger()}}).invalidAPIRequest(c, err); err != nil {
					t.Fatal(err)
				}
				var response hubapi.APIErrorResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if recorder.Code != http.StatusUnprocessableEntity || response.Code != "invalid_request" || !strings.Contains(response.Message, test.field) || strings.Contains(response.Message, "private value") {
					t.Fatalf("invalid body response = %d %+v", recorder.Code, response)
				}
			}
			if !test.wantErr && heartbeat.Version != "develop-040e7195c" {
				t.Fatalf("version = %q", heartbeat.Version)
			}
			if !test.wantErr && heartbeat.ProjectConfiguration.ProjectID != test.project {
				t.Fatalf("project ID = %q", heartbeat.ProjectConfiguration.ProjectID)
			}
		})
	}
}

func TestAPIRefusals(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	f := accountPreferencesPostgresFixture(t, repository)
	f.service.config.deps.NewTokenID = func() string { return hubapi.NewID("credential") }
	for _, organization := range []tracker.OrganizationID{"org_security", "org_other"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}); err != nil {
				return err
			}
			return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "org_provider"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: owner.identity.Hosted, Email: owner.identity.Email}, apikey.HashToken(owner.token))
	if err != nil {
		t.Fatal(err)
	}
	keyRefusal := func(token bool) func(*Service, echo.Context) error {
		return func(s *Service, c echo.Context) error {
			var request hostedKeyRequest
			if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
				return err
			}
			var err error
			if token {
				_, err = f.service.createAPITokenFor(c.Request().Context(), hubapi.TokenRequest{Name: request.Name, Scope: hubapi.APIScopeOperator, Issuer: &credential, KeyScope: request.Scope, ProjectAccess: request.ProjectAccess, ProjectIDs: request.Projects})
			} else {
				_, err = f.service.createHostedAPIKeyFor(c.Request().Context(), credential, request)
			}
			return s.nativeAPIError(c, err)
		}
	}
	budgetRefusal := func(s *Service, c echo.Context) error {
		var request monthlyBudgetRequest
		if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
			return err
		}
		if err := request.Policy.Validate(); err != nil {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid(err.Error()))
		}
		return nil
	}
	for _, test := range []struct {
		name, path, body, code, reason string
		status                         int
		handle                         func(*Service, echo.Context) error
	}{
		{"budget mode", hubapi.NativeBase + "/monthly-budget", `{"policy":{"mode":"private-token"}}`, "invalid_request", "monthly budget mode must be drain_issue or hard_stop", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative Luna budget", hubapi.NativeBase + "/monthly-budget", `{"policy":{"luna_api_micros":-1}}`, "invalid_request", "monthly luna_api budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative runner budget", hubapi.NativeBase + "/monthly-budget", `{"policy":{"runner_api_micros":-1}}`, "invalid_request", "monthly runner_api budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative sprite budget", hubapi.NativeBase + "/monthly-budget", `{"policy":{"sprite_infrastructure_micros":-1}}`, "invalid_request", "monthly sprite_infrastructure budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative total budget", hubapi.NativeBase + "/monthly-budget", `{"policy":{"total_micros":-1}}`, "invalid_request", "monthly total budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"hosted key access", "/api/v2/organizations/:organization/api-keys", `{"name":"private-token","scope":"read","never_expires":true,"project_access":"private-token"}`, "invalid_request", "Choose all or selected project access", http.StatusUnprocessableEntity, keyRefusal(false)},
		{"hosted key selected projects", "/api/v2/organizations/:organization/api-keys", `{"name":"private-token","scope":"read","never_expires":true,"project_access":"selected"}`, "invalid_request", "Selected project access requires at least one project", http.StatusUnprocessableEntity, keyRefusal(false)},
		{"hosted key all projects", "/api/v2/organizations/:organization/api-keys", `{"name":"private-token","scope":"read","never_expires":true,"project_access":"all","project_ids":["private-project"]}`, "invalid_request", "All project access cannot include selected project IDs", http.StatusUnprocessableEntity, keyRefusal(false)},
		{"token access", "/api/v1/tokens", `{"name":"private-token","scope":"read","project_access":"private-token"}`, "invalid_request", "Choose all or selected project access", http.StatusUnprocessableEntity, keyRefusal(true)},
		{"token selected projects", "/api/v1/tokens", `{"name":"private-token","scope":"read","project_access":"selected"}`, "invalid_request", "Selected project access requires at least one project", http.StatusUnprocessableEntity, keyRefusal(true)},
		{"token all projects", "/api/v1/tokens", `{"name":"private-token","scope":"read","project_access":"all","project_ids":["private-project"]}`, "invalid_request", "All project access cannot include selected project IDs", http.StatusUnprocessableEntity, keyRefusal(true)},
		{"attachment metadata", hubapi.NativeBase + "/attempts/:attempt/attachment-metadata", `{}`, "invalid_request", "Invalid attachment metadata", http.StatusUnprocessableEntity, (*Service).recordCloudAttachment},
		{"archive query", hubapi.NativeBase + "/work-items?archive_url=private-token", "", "invalid_request", "archive_url supports true or false", http.StatusUnprocessableEntity, (*Service).listNativeIssues},
		{"repeated archive query", hubapi.NativeBase + "/work-items?archive_url=true&archive_url=false", "", "invalid_request", "archive_url supports true or false", http.StatusUnprocessableEntity, (*Service).listNativeIssues},
		{"archive reference", attachmentMetadataBase + "/archives/:item", `{}`, "invalid_request", "Invalid archive reference", http.StatusUnprocessableEntity, (*Service).recordAttachmentArchive},
		{"diff generation sequence", hubapi.NativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"attempt","seq":0}}`, "invalid_request", "invalid tracker work event: generation seq must be positive", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"diff attempt generation ID", hubapi.NativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"attempt","id":"private-token","seq":1}}`, "invalid_request", "invalid tracker work event: an attempt generation carries no id", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"diff workspace generation ID", hubapi.NativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"workspace","seq":1}}`, "invalid_request", "invalid tracker work event: a workspace generation requires its workspace id", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"diff generation source", hubapi.NativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"private-token","seq":1}}`, "invalid_request", "invalid tracker work event: generation source must be attempt or workspace", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"attempt evidence", hubapi.NativeBase + "/attempts/:attempt/evidence", "", "invalid_request", "Invalid attempt evidence", http.StatusUnprocessableEntity, func(s *Service, c echo.Context) error {
			c.SetParamNames("attempt")
			c.SetParamValues("attempt_example")
			return s.nativeAPIError(c, s.checkAttachmentEvidence(c, nil))
		}},
		{"claim ID", hubapi.NativeBase + "/claims", `{"work_item_id":-1}`, "invalid_request", "Request body contains an invalid field type", http.StatusUnprocessableEntity, (*Service).claimNativeIssue},
		{"claim body limit", hubapi.NativeBase + "/claims", `{"session_id":"` + strings.Repeat("x", hubapi.MaxAPIRequestBodyBytes) + `"}`, "payload_too_large", "Request body is too large", http.StatusRequestEntityTooLarge, (*Service).claimNativeIssue},
		{"budget override body limit", hubapi.NativeBase + "/budget-override", `{"idempotency_key":"budget-limit","reason":"` + strings.Repeat("x", hubapi.MaxAPIRequestBodyBytes) + `"}`, "payload_too_large", "Request body is too large", http.StatusRequestEntityTooLarge, func(s *Service, c echo.Context) error {
			c.Request().Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			return s.putBudgetOverride(c)
		}},
		{"claim TTL", hubapi.NativeBase + "/claims", `{"ttl_seconds":-1,"protocol_major":2,"capabilities":["native_issues","scoped_collaboration"]}`, "invalid_request", "Lease TTL is invalid", http.StatusUnprocessableEntity, (*Service).claimNativeIssue},
		{"dependency key", hubapi.NativeBase + "/work-items/:item/dependencies", `{}`, "invalid_request", "An idempotency key of at most 128 bytes is required", http.StatusUnprocessableEntity, (*Service).changeNativeDependency},
		{"key administration", "/internal/keys/admin", "", "forbidden", "Organization administrator service access is required", http.StatusForbidden, (*Service).entryKeyAdmin},
		{"key context", "/internal/keys/context", "", "forbidden", "Shared service identity is required to read key context", http.StatusForbidden, (*Service).entryKeyContext},
		{"key audit", "/internal/keys/audit", `{"key_id":"key_example","owner":"private-token","event":"created"}`, "forbidden", "Shared service access is required to record key audit events", http.StatusForbidden, (*Service).entryKeyAudit},
		{"credit balance", hubapi.NativeBase + "/ai-credits", "", "forbidden", "Shared service access is required to read AI credits", http.StatusForbidden, func(s *Service, c echo.Context) error { return credits.NewHTTP(s.creditHTTPDeps()).PlatformBalance(c) }},
		{"credit adjustment", hubapi.NativeBase + "/ai-credits", "", "forbidden", "Shared service identity and email are required to adjust AI credits", http.StatusForbidden, func(s *Service, c echo.Context) error { return credits.NewHTTP(s.creditHTTPDeps()).AdjustPlatform(c) }},
		{"request metrics", hubapi.NativeBase + "/request-metrics", "", "forbidden", "Staff access is required to read request metrics", http.StatusForbidden, (*Service).hostedRequestMetrics},
		{"runtime read", hubapi.NativeBase + "/runtime", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, func(s *Service, c echo.Context) error {
			return hostedplans.NewHTTP(s.hostedPlansHTTPDeps()).ReadRuntime(c)
		}},
		{"runtime reload", hubapi.NativeBase + "/runtime", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, func(s *Service, c echo.Context) error {
			return hostedplans.NewHTTP(s.hostedPlansHTTPDeps()).ReloadRuntime(c)
		}},
		{"plan read", hubapi.NativeBase + "/plan", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, func(s *Service, c echo.Context) error {
			return hostedplans.NewHTTP(s.hostedPlansHTTPDeps()).PlanReport(c)
		}},
		{"plan update", hubapi.NativeBase + "/plan", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, func(s *Service, c echo.Context) error {
			return hostedplans.NewHTTP(s.hostedPlansHTTPDeps()).UpdatePlan(c)
		}},
		{"billing webhook", "/webhooks/stripe", `{"private-token":"test-secret"}`, "invalid_request", "Billing webhook signature, timestamp or mode is invalid", http.StatusBadRequest, func(s *Service, c echo.Context) error {
			s.database = &database{}
			s.config.Clock = time.Now
			s.config.Hosted.Billing = &hostedplans.HostedBillingConfig{WebhookSecret: []byte("test-secret")}
			return s.hostedStripeWebhook(c)
		}},
		{"operator writer", "/api/v1/work-items/:id/workflow", "", "idempotency_conflict", "Idempotency key conflicts with an earlier mutation", http.StatusConflict, func(s *Service, c echo.Context) error {
			return s.operatorAPIError(c, ErrIdempotencyConflict)
		}},
		{"hosted writer", hubapi.NativeBase, "", "forbidden", "Owner access is required", http.StatusForbidden, func(s *Service, c echo.Context) error {
			return s.hostedJSONError(c, http.StatusForbidden, "Owner access is required")
		}},
		{"billing writer", hubapi.NativeBase + "/billing", "", "billing_conflict", "A billing account is required", http.StatusConflict, func(s *Service, c echo.Context) error {
			return s.hostedBillingFailure(c, true, http.StatusConflict, "A billing account is required")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			service := &Service{config: Config{AccessRepository: repository, TenantRepository: repository, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), GitHubWebhookSecret: []byte("test-secret"), Hosted: &hostedplans.HostedConfig{}}}
			recorded := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body)), recorded)
			c.SetPath(strings.SplitN(test.path, "?", 2)[0])
			c.SetParamNames("id", "organization", "project", "attempt")
			c.SetParamValues("1", "private-organization", "private-project", "attempt_00000000000000000000000000000001")
			if err := test.handle(service, c); err != nil {
				t.Fatal(err)
			}
			failure := requireNativeCode(t, recorded, test.status, test.code)
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
				t.Fatalf("expected exactly one JSON log entry: %v: %s", err, &logs)
			}
			message := test.reason
			if test.name == "claim ID" {
				message = "Request body is invalid: invalid type for field work_item_id"
			}
			if entry["level"] != "WARN" || entry["code"] != test.code || entry["reason"] != test.reason || entry["error"] != test.reason || entry["route"] != c.Path() || failure.Message != message {
				t.Fatalf("refusal = %#v, log = %#v", failure, entry)
			}
			if entry["organization_id"] != "" || entry["project_id"] != "" {
				t.Fatalf("unvalidated scope in log = %#v", entry)
			}
			for _, private := range []string{"private-token", "test-secret", "private-organization", "private-project"} {
				if strings.Contains(logs.String(), private) || strings.Contains(recorded.Body.String(), private) {
					t.Fatalf("private context escaped: %s", private)
				}
			}
		})
	}
	var logs hostedLogBuffer
	native := newDefaultNativeFixture(t, Config{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	first, second := native.create(t, "first refusal item"), native.create(t, "second refusal item")
	for _, test := range []struct {
		name, method, path, body, message string
	}{
		{"private query", http.MethodGet, native.base + "/work-items?priority=private-token", "", "priority must be between zero and three"},
		{"dependency action", http.MethodPost, native.base + "/work-items/" + string(first.WorkItemID) + "/dependencies", fmt.Sprintf(`{"idempotency_key":"invalid-operation","expected_revision":"1","related_work_item_id":%q,"operation":"private-token"}`, second.WorkItemID), "Relationship operation must be add or remove"},
		{"dependency distinct item", http.MethodPost, native.base + "/work-items/" + string(first.WorkItemID) + "/dependencies", fmt.Sprintf(`{"idempotency_key":"self-dependency","expected_revision":"1","related_work_item_id":%q,"operation":"add"}`, first.WorkItemID), "An issue cannot be related to itself"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logStart := len(logs.String())
			response := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			request.Header.Set(echo.HeaderAuthorization, "Bearer "+native.token)
			native.service.Handler().ServeHTTP(response, request)
			failure := requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request")
			logged := logs.String()[logStart:]
			route := hubapi.NativeBase + "/work-items"
			if strings.Contains(test.path, "/dependencies") {
				route += "/:item/dependencies"
			}
			var entry map[string]any
			matched := 0
			for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
				if line == "" {
					continue
				}
				var candidate map[string]any
				if err := json.Unmarshal([]byte(line), &candidate); err != nil {
					t.Fatalf("invalid JSON log: %v: %s", err, line)
				}
				if candidate["level"] == "WARN" && candidate["route"] == route && candidate["method"] == test.method && candidate["refusal_code"] == "invalid_request" {
					entry = candidate
					matched++
				}
			}
			if matched != 1 {
				t.Fatalf("matching request refusal warnings=%d, want1: %s", matched, logged)
			}
			if entry["route"] != route || entry["organization_id"] != string(native.project.OrganizationID) || entry["project_id"] != string(native.project.ID) {
				t.Fatalf("validated refusal scope or route = %#v", entry)
			}
			if failure.Message != test.message || entry["level"] != "WARN" || entry["reason"] != test.message || entry["error"] != test.message || entry["refusal_code"] != "invalid_request" {
				t.Fatalf("refusal = %#v, log = %#v", failure, entry)
			}
			if code, present := entry["code"]; present {
				t.Fatalf("hosted refusal log kept code %#v: %#v", code, entry)
			}
			if strings.Contains(logged+response.Body.String(), "private-token") {
				t.Fatal("private query or operation escaped in refusal")
			}
		})
	}
	native.service.echo = echo.New()
	native.service.registerRoutes(native.service.echo)
	for _, test := range []struct{ name, method, path string }{
		{"retired backup", http.MethodPost, "/api/v1/backup"},
		{"retired ordering", http.MethodPost, "/api/v1/work-items/1/order"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeCode(t, performHubAPIRequest(t, native.service, test.method, test.path, native.token, map[string]string{}), http.StatusNotFound, "not_found")
		})
	}
}

func TestInternalAPIErrorRedactsPostgresDetails(t *testing.T) {
	t.Parallel()
	for _, schema := range []string{"public", "private-schema"} {
		t.Run(schema, func(t *testing.T) {
			cause := &pgconn.PgError{Code: "23502", SchemaName: schema, TableName: "invalid.private-table", ColumnName: "private column", Message: "private message", Detail: "private detail", Hint: "private hint", InternalQuery: "private query"}
			err := pgcore.MapError(t.Context(), cause)
			var logs bytes.Buffer
			service := &Service{config: Config{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}}
			response := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/example", nil), response)
			if err := service.internalAPIError(c, "example_unavailable", "Example unavailable", err); err != nil {
				t.Fatal(err)
			}
			requireNativeCode(t, response, http.StatusInternalServerError, "example_unavailable")
			if strings.Contains(logs.String()+response.Body.String(), "private") {
				t.Fatal("database error exposed private identifiers or content")
			}
		})
	}
}
