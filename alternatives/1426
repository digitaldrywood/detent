package hubserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
)

func TestDecodeAPIJSONCompatibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		scope   apiScope
		body    string
		wantErr bool
		project string
		path    string
		field   string
	}{
		{name: "worker future heartbeat field", scope: apiScopeWorker, body: `{"version":"develop-040e7195c","future_observation":true}`},
		{name: "worker newer project configuration", scope: apiScopeWorker, body: `{"version":"develop-040e7195c","project_configuration":{"project_id":"project","policy_mismatch":false}}`, project: "project"},
		{name: "operator unknown field", scope: apiScopeOperator, body: `{"runner_version":"test"}`, wantErr: true, field: "runner_version"},
		{name: "admin nested unknown field", scope: apiScopeAdmin, body: `{"project_configuration":{"project_name":"project"}}`, wantErr: true, field: "project_name"},
		{name: "policy extra retry key", scope: apiScopeAdmin, path: nativeBase + "/onboarding/policy", body: `{"idempotency_key":"private value"}`, wantErr: true, field: "idempotency_key"},
		{name: "unauthenticated unknown field", body: `{"future_observation":true}`, wantErr: true},
		{name: "worker invalid known field", scope: apiScopeWorker, body: `{"version":42}`, wantErr: true, field: "version"},
		{name: "worker unknown run event field", scope: apiScopeWorker, path: nativeBase + "/work-items/:item/events", body: `{"version":"develop-040e7195c","prompt":"private prompt"}`, wantErr: true},
		{name: "worker multiple values", scope: apiScopeWorker, body: `{"version":"test"} {}`, wantErr: true},
		{name: "worker malformed JSON", scope: apiScopeWorker, body: `{"version":`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body)), recorder)
			path := test.path
			if path == "" {
				path = nativeBase + "/machines/:machine/heartbeat"
			}
			c.SetPath(path)
			if test.scope != "" {
				c.Set("hub_api_credential", apiCredential{Scope: test.scope})
			}
			var heartbeat struct {
				Version              string `json:"version"`
				ProjectConfiguration struct {
					ProjectID string `json:"project_id"`
				} `json:"project_configuration"`
			}
			err := decodeAPIJSON(c, &heartbeat)
			if (err != nil) != test.wantErr {
				t.Fatalf("decode error = %v, want error %t", err, test.wantErr)
			}
			if test.field != "" {
				if err := (&Service{config: Config{Logger: discardLogger()}}).invalidAPIRequest(c, err); err != nil {
					t.Fatal(err)
				}
				var response apiErrorResponse
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
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: owner.identity.Hosted, Email: owner.identity.Email}, apikey.HashToken(owner.token))
	if err != nil {
		t.Fatal(err)
	}
	keyRefusal := func(token bool) func(*Service, echo.Context) error {
		return func(s *Service, c echo.Context) error {
			var request hostedKeyRequest
			if err := decodeAPIJSON(c, &request); err != nil {
				return err
			}
			var err error
			if token {
				_, err = f.service.createAPITokenFor(c.Request().Context(), tokenRequest{Name: request.Name, Scope: apiScopeOperator, Issuer: &credential, KeyScope: request.Scope, ProjectAccess: request.ProjectAccess, ProjectIDs: request.Projects})
			} else {
				_, err = f.service.createHostedAPIKeyFor(c.Request().Context(), credential, request)
			}
			return s.nativeAPIError(c, err)
		}
	}
	budgetRefusal := func(s *Service, c echo.Context) error {
		var request monthlyBudgetRequest
		if err := decodeAPIJSON(c, &request); err != nil {
			return err
		}
		_, err := updateMonthlyBudgetOperation(request)(c.Request().Context(), nil, nativeScope{}, time.Time{})
		return s.nativeAPIError(c, err)
	}
	for _, test := range []struct {
		name, path, body, code, reason string
		status                         int
		handle                         func(*Service, echo.Context) error
	}{
		{"budget mode", nativeBase + "/monthly-budget", `{"policy":{"mode":"private-token"}}`, "invalid_request", "monthly budget mode must be drain_issue or hard_stop", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative Luna budget", nativeBase + "/monthly-budget", `{"policy":{"luna_api_micros":-1}}`, "invalid_request", "monthly luna_api budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative runner budget", nativeBase + "/monthly-budget", `{"policy":{"runner_api_micros":-1}}`, "invalid_request", "monthly runner_api budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative sprite budget", nativeBase + "/monthly-budget", `{"policy":{"sprite_infrastructure_micros":-1}}`, "invalid_request", "monthly sprite_infrastructure budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"negative total budget", nativeBase + "/monthly-budget", `{"policy":{"total_micros":-1}}`, "invalid_request", "monthly total budget must be nonnegative", http.StatusUnprocessableEntity, budgetRefusal},
		{"hosted key access", "/api/v2/organizations/:organization/api-keys", `{"name":"private-token","scope":"read","never_expires":true,"project_access":"private-token"}`, "invalid_request", "Choose all or selected project access", http.StatusUnprocessableEntity, keyRefusal(false)},
		{"hosted key selected projects", "/api/v2/organizations/:organization/api-keys", `{"name":"private-token","scope":"read","never_expires":true,"project_access":"selected"}`, "invalid_request", "Selected project access requires at least one project", http.StatusUnprocessableEntity, keyRefusal(false)},
		{"hosted key all projects", "/api/v2/organizations/:organization/api-keys", `{"name":"private-token","scope":"read","never_expires":true,"project_access":"all","project_ids":["private-project"]}`, "invalid_request", "All project access cannot include selected project IDs", http.StatusUnprocessableEntity, keyRefusal(false)},
		{"token access", "/api/v1/tokens", `{"name":"private-token","scope":"read","project_access":"private-token"}`, "invalid_request", "Choose all or selected project access", http.StatusUnprocessableEntity, keyRefusal(true)},
		{"token selected projects", "/api/v1/tokens", `{"name":"private-token","scope":"read","project_access":"selected"}`, "invalid_request", "Selected project access requires at least one project", http.StatusUnprocessableEntity, keyRefusal(true)},
		{"token all projects", "/api/v1/tokens", `{"name":"private-token","scope":"read","project_access":"all","project_ids":["private-project"]}`, "invalid_request", "All project access cannot include selected project IDs", http.StatusUnprocessableEntity, keyRefusal(true)},
		{"outbox limit", "/api/v1/outbox/health?limit=0", "", "invalid_query", "limit must be between 1 and 200", http.StatusUnprocessableEntity, (*Service).outboxHealth},
		{"backup destination", "/api/v1/backup", `{}`, "invalid_request", "A new absolute backup output path is required", http.StatusUnprocessableEntity, (*Service).backupDatabase},
		{"attachment metadata", nativeBase + "/attempts/:attempt/attachment-metadata", `{}`, "invalid_request", "Invalid attachment metadata", http.StatusUnprocessableEntity, (*Service).recordCloudAttachment},
		{"archive query", nativeBase + "/work-items?archive_url=private-token", "", "invalid_request", "archive_url supports true or false", http.StatusUnprocessableEntity, (*Service).listNativeIssues},
		{"repeated archive query", nativeBase + "/work-items?archive_url=true&archive_url=false", "", "invalid_request", "archive_url supports true or false", http.StatusUnprocessableEntity, (*Service).listNativeIssues},
		{"archive reference", attachmentMetadataBase + "/archives/:item", `{}`, "invalid_request", "Invalid archive reference", http.StatusUnprocessableEntity, (*Service).recordAttachmentArchive},
		{"diff generation sequence", nativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"attempt","seq":0}}`, "invalid_request", "invalid tracker work event: generation seq must be positive", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"diff attempt generation ID", nativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"attempt","id":"private-token","seq":1}}`, "invalid_request", "invalid tracker work event: an attempt generation carries no id", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"diff workspace generation ID", nativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"workspace","seq":1}}`, "invalid_request", "invalid tracker work event: a workspace generation requires its workspace id", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"diff generation source", nativeBase + "/attempts/:attempt/diff", `{"generation":{"source":"private-token","seq":1}}`, "invalid_request", "invalid tracker work event: generation source must be attempt or workspace", http.StatusUnprocessableEntity, (*Service).postAttemptDiff},
		{"attempt evidence", nativeBase + "/attempts/:attempt/evidence", "", "invalid_request", "Invalid attempt evidence", http.StatusUnprocessableEntity, func(s *Service, c echo.Context) error {
			c.SetParamNames("attempt")
			c.SetParamValues("attempt_example")
			return s.nativeAPIError(c, s.checkAttachmentEvidence(c, nil))
		}},
		{"outbox cursor", "/api/v1/outbox/health?cursor=private-token", "", "invalid_cursor", "Outbox cursor is invalid", http.StatusUnprocessableEntity, (*Service).outboxHealth},
		{"claim ID", "/api/v1/claims", `{"work_item_id":-1}`, "invalid_claim", "work_item_id must be positive when supplied", http.StatusUnprocessableEntity, (*Service).claimWorkItem},
		{"claim TTL", "/api/v1/claims", `{"ttl_seconds":-1}`, "invalid_claim", "ttl_seconds must be between 1 and 86400", http.StatusUnprocessableEntity, (*Service).claimWorkItem},
		{"dependency", "/api/v1/work-items/:id/dependencies", `{}`, "invalid_dependency", "Dependency action and a distinct positive blocker_work_item_id are required", http.StatusUnprocessableEntity, (*Service).changeWorkItemDependency},
		{"order", "/api/v1/work-items/:id/order", `{}`, "invalid_order", "Scope, state, and rank are required", http.StatusUnprocessableEntity, (*Service).changeWorkItemOrder},
		{"private query", "/api/v1/work-items?priority=private-token", "", "invalid_query", "priority filter value is invalid", http.StatusUnprocessableEntity, (*Service).listWorkItems},
		{"key administration", "/internal/keys/admin", "", "forbidden", "Organization administrator service access is required", http.StatusForbidden, (*Service).entryKeyAdmin},
		{"key context", "/internal/keys/context", "", "forbidden", "Shared service identity is required to read key context", http.StatusForbidden, (*Service).entryKeyContext},
		{"key audit", "/internal/keys/audit", `{"key_id":"key_example","owner":"private-token","event":"created"}`, "forbidden", "Shared service access is required to record key audit events", http.StatusForbidden, (*Service).entryKeyAudit},
		{"credit balance", nativeBase + "/ai-credits", "", "forbidden", "Shared service access is required to read AI credits", http.StatusForbidden, (*Service).platformTenantAICreditBalance},
		{"credit adjustment", nativeBase + "/ai-credits", "", "forbidden", "Shared service identity and email are required to adjust AI credits", http.StatusForbidden, (*Service).adjustPlatformAICredits},
		{"request metrics", nativeBase + "/request-metrics", "", "forbidden", "Staff access is required to read request metrics", http.StatusForbidden, (*Service).hostedRequestMetrics},
		{"runtime read", nativeBase + "/runtime", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, (*Service).readHostedRuntime},
		{"runtime reload", nativeBase + "/runtime", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, (*Service).reloadHostedRuntime},
		{"plan read", nativeBase + "/plan", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, (*Service).hostedPlanReport},
		{"plan update", nativeBase + "/plan", "", "forbidden", "Entitlement administrator access is required", http.StatusForbidden, (*Service).updateHostedPlan},
		{"billing webhook", "/webhooks/stripe", `{"private-token":"test-secret"}`, "invalid_request", "Billing webhook signature, timestamp or mode is invalid", http.StatusBadRequest, func(s *Service, c echo.Context) error {
			s.database = &database{}
			s.config.now = time.Now
			s.config.Hosted.Billing = &HostedBillingConfig{WebhookSecret: []byte("test-secret")}
			return s.hostedStripeWebhook(c)
		}},
		{"webhook headers", "/webhooks/github", `{}`, "invalid_headers", "GitHub webhook delivery and event headers are required", http.StatusBadRequest, (*Service).githubWebhook},
		{"operator writer", "/api/v1/work-items/:id/workflow", "", "idempotency_conflict", "Idempotency key conflicts with an earlier mutation", http.StatusConflict, func(s *Service, c echo.Context) error {
			return s.operatorAPIError(c, ErrIdempotencyConflict)
		}},
		{"hosted writer", nativeBase, "", "forbidden", "Owner access is required", http.StatusForbidden, func(s *Service, c echo.Context) error {
			return s.hostedJSONError(c, http.StatusForbidden, "Owner access is required")
		}},
		{"billing writer", nativeBase + "/billing", "", "billing_conflict", "A billing account is required", http.StatusConflict, func(s *Service, c echo.Context) error {
			return s.hostedBillingFailure(c, true, http.StatusConflict, "A billing account is required")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			service := &Service{config: Config{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), GitHubWebhookSecret: []byte("test-secret"), Hosted: &HostedConfig{}}}
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
			if entry["level"] != "WARN" || entry["code"] != test.code || entry["reason"] != test.reason || entry["error"] != test.reason || entry["route"] != c.Path() || failure.Message != test.reason {
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
}
