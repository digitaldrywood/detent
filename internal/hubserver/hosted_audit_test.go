package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedAuditSecurityEvents(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "owner", "owner", "owner@example.test", "write", "support@example.test")
	for _, test := range []struct {
		name, event, route      string
		mutation, keep, support bool
		status                  int
	}{
		{name: "HTTP read", event: "action", route: "GET /projects"},
		{name: "HTTP head", event: "action", route: "HEAD /projects"},
		{name: "HTTP options", event: "action", route: "OPTIONS /projects"},
		{name: "administration read", event: "administration", route: "GET /runners"},
		{name: "MCP read", event: "action", route: "mcp hosted_events"},
		{name: "billing view", event: "billing_viewed", route: "/billing"},
		{name: "billing export", event: "billing_exported", route: "mcp.billing_export"},
		{name: "sign in", event: "session_started", route: "/auth/oidc/callback", keep: true},
		{name: "sign out", event: "session_ended", route: "/logout", keep: true},
		{name: "token creation", event: "credential_created", route: "POST /api-keys", keep: true},
		{name: "token revocation", event: "credential_revoked", route: "DELETE /api-keys/:key", keep: true},
		{name: "membership change", event: "administration", route: "POST /members", keep: true},
		{name: "role change", event: "administration", route: "PUT /members/:member", keep: true},
		{name: "grant change", event: "administration", route: "DELETE /grants/:grant", keep: true},
		{name: "policy approval", event: "action", route: "POST /policy/approve", keep: true},
		{name: "HTTP mutation", event: "action", route: "PATCH /work-items/:item", keep: true},
		{name: "MCP mutation", event: "action", route: "mcp update_issue", mutation: true, keep: true},
		{name: "billing mutation", event: "succeeded", route: "billing_change", mutation: true, keep: true},
		{name: "read refusal", event: "action", route: "GET /denied", status: http.StatusForbidden, keep: true},
		{name: "read error", event: "action", route: "GET /failed", status: http.StatusInternalServerError, keep: true},
		{name: "impersonation", event: "support_started", route: "POST /support", support: true, keep: true},
		{name: "support read", event: "action", route: "GET /support", support: true, keep: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			if test.mutation {
				ctx = mutation.WithContext(ctx, mutation.Metadata{Action: "update", Source: "mcp", CorrelationID: "correlation", RetryIdentity: "private-retry", InputHash: "private-input"})
			}
			identity := *user.identity.Hosted
			if !test.support && !test.mutation {
				identity.SupportActor, identity.SupportReason = "", ""
			}
			status := test.status
			if status == 0 {
				status = http.StatusOK
			}
			if err := f.service.hostedAudit(ctx, &identity, test.event, test.route, string(f.project), status); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND event=$2 AND route=$3", f.service.accessOrganization(), test.event, test.route).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.keep {
				want = 1
			}
			if count != want {
				t.Fatalf("audit rows=%d, want %d", count, want)
			}
			if test.mutation {
				var id int64
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT id FROM hosted_audit WHERE organization_id=$1 AND event=$2 AND route=$3", f.service.accessOrganization(), test.event, test.route).Scan(&id); err != nil {
					t.Fatal(err)
				}
				var audit accessstore.Audit
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					audit, err = r.AccessAudit(t.Context(), id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if audit.Mutation == nil {
					t.Fatal("mutation audit has no archived metadata")
				}
				raw, err := f.service.runDetailStore().ReadDetail(t.Context(), string(audit.OrganizationID), *audit.Mutation)
				if err != nil {
					t.Fatal(err)
				}
				var summary struct {
					Action string `json:"action"`
					Retry  any    `json:"retry_identity"`
					Input  any    `json:"input_hash"`
				}
				if err := json.Unmarshal(raw, &summary); err != nil {
					t.Fatal(err)
				}
				if audit.ActualActor != "support@example.test" || audit.EffectiveUser != user.identity.Subject || summary.Action != "update" || summary.Retry != nil || summary.Input != nil {
					t.Fatalf("mutation audit actor=%s effective=%s action=%s retry=%v input=%v", audit.ActualActor, audit.EffectiveUser, summary.Action, summary.Retry, summary.Input)
				}
			}
		})
	}
}

func TestHostedAuditRetention(t *testing.T) {
	t.Run("portable cutoffs across organizations", func(t *testing.T) {
		f := newHostedSecurityFixture(t)
		if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		repository := f.service.config.TenantRepository
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		organizations := []tracker.OrganizationID{f.service.accessOrganization(), "org_other_time"}
		if err := repository.WriteTenant(t.Context(), organizations[1], corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organizations[1], Name: "Other organization", Slug: "other-audit", CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		ids := make(map[tracker.OrganizationID]map[string]int64)
		stamps := make(map[string]time.Time)
		for _, org := range organizations {
			ids[org] = make(map[string]int64)
			for _, test := range []struct{ name, at string }{
				{"rollover", "2026-09-21T11:59:59.9996Z"},
				{"last nanosecond", "2026-09-21T11:59:59.999999999Z"},
				{"equal", "2026-09-21T12:00:00Z"},
				{"offset equal", "2026-09-21T07:00:00-05:00"},
				{"retained nanosecond", "2026-09-21T12:00:00.000000001Z"},
			} {
				at, err := hubapi.ParseTimeValue(test.at)
				if err != nil {
					t.Fatal(err)
				}
				stamps[test.name] = at
				ids[org][test.name] = seedHostedAuditRecord(t, f.service, accessstore.Audit{OrganizationID: org, SessionID: "session", ActualActor: "actor", EffectiveUser: "actor", Event: test.name, Status: 200, StartedAt: now, ExpiresAt: now, RecordedAt: at, RecordedAtNS: at.UnixNano()})
			}
		}
		var rows, typed int
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM hosted_audit WHERE organization_id=ANY($1::text[])),(SELECT count(*) FROM hosted_audit_times WHERE organization_id=ANY($1::text[]))`, []string{string(organizations[0]), string(organizations[1])}).Scan(&rows, &typed); err != nil {
			t.Fatal(err)
		}
		if rows != 10 || typed != 10 {
			t.Fatalf("typed input rows=%d typed=%d want 10/10", rows, typed)
		}
		for pass := range 3 {
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			for _, org := range organizations {
				var retained []string
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					for _, name := range []string{"rollover", "last nanosecond", "equal", "offset equal", "retained nanosecond"} {
						row, err := r.AccessAudit(t.Context(), ids[org][name])
						if name == "rollover" || name == "last nanosecond" {
							if !errors.Is(err, corestore.ErrNotFound) {
								return fmt.Errorf("expired %s audit survived: %v", name, err)
							}
							continue
						}
						if err != nil {
							return err
						}
						if row.OrganizationID != org || row.RecordedAtNS != stamps[name].UnixNano() {
							return fmt.Errorf("audit time or tenant changed: %+v", row)
						}
						if name == "offset equal" && !row.RecordedAt.Equal(stamps[name]) {
							return fmt.Errorf("offset instant changed: %s", row.RecordedAt)
						}
						retained = append(retained, row.Event)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if events := strings.Join(retained, ","); events != "equal,offset equal,retained nanosecond" {
					t.Fatalf("pass%d %s retained %s", pass, org, events)
				}
			}
		}
		for index, org := range organizations {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for i := index; i < commandReceiptPruneBatch+7; i += len(organizations) {
					at := now.Add(-hostedAuditRetention - time.Duration(i+1)*time.Nanosecond)
					if _, err := w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: org, SessionID: "global_backlog", ActualActor: "actor", EffectiveUser: "actor", Event: "backlog", Status: 200, StartedAt: now, ExpiresAt: now, RecordedAt: at, RecordedAtNS: at.UnixNano()}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for pass, want := range []int{7, 0, 0} {
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			var remaining int
			var oldest sql.NullInt64
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*),min(recorded_at_ns) FROM hosted_audit WHERE organization_id=ANY($1::text[]) AND session_id='global_backlog'`, []string{string(organizations[0]), string(organizations[1])}).Scan(&remaining, &oldest); err != nil {
				t.Fatal(err)
			}
			if remaining != want || (want > 0 && (!oldest.Valid || oldest.Int64 != now.Add(-hostedAuditRetention).UnixNano()-int64(want))) {
				t.Fatalf("global pass%d remaining=%d oldest=%+v want%d oldest-first", pass, remaining, oldest, want)
			}
		}
		var dangling int
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit_times t LEFT JOIN hosted_audit a ON a.organization_id=t.organization_id AND a.id=t.audit_id WHERE a.id IS NULL`).Scan(&dangling); err != nil || dangling != 0 {
			t.Fatalf("dangling indexes=%d err=%v", dangling, err)
		}
	})
	t.Parallel()
	f := newHostedSecurityFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 22, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, test := range []struct {
		event string
		at    time.Time
	}{
		{"31 days", now.Add(-31 * day)},
		{"just expired", now.Add(-30*day - time.Second)},
		{"30 days", now.Add(-30 * day)},
		{"29 days", now.Add(-29 * day)},
		{"recent", now},
	} {
		seedHostedAuditRecord(t, f.service, accessstore.Audit{OrganizationID: f.service.accessOrganization(), SessionID: "session", ActualActor: "actor", EffectiveUser: "actor", Event: test.event, Status: 200, StartedAt: now, ExpiresAt: now.Add(time.Hour), RecordedAt: test.at, RecordedAtNS: test.at.UnixNano()})
	}
	backlog := commandReceiptPruneBatch + 7
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		for i := range backlog {
			at := now.Add(-45 * day)
			if _, err := w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: f.service.accessOrganization(), SessionID: "session", ActualActor: "actor", EffectiveUser: "actor", Event: fmt.Sprintf("backlog %d", i), Status: 200, StartedAt: now, ExpiresAt: now.Add(time.Hour), RecordedAt: at, RecordedAtNS: at.UnixNano()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		backlog int
		events  string
	}{
		{7, "31 days,just expired,30 days,29 days,recent"},
		{0, "30 days,29 days,recent"},
		{0, "30 days,29 days,recent"},
	} {
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		var remaining int
		var events string
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND event LIKE 'backlog%'`, f.service.accessOrganization()).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT COALESCE(string_agg(event,',' ORDER BY id),'') FROM hosted_audit WHERE organization_id=$1 AND event NOT LIKE 'backlog%'`, f.service.accessOrganization()).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if remaining != want.backlog || events != want.events {
			t.Fatalf("retained events=%s backlog=%d want %+v", events, remaining, want)
		}
	}
}

func TestHostedAuditLegacyReadCleanup(t *testing.T) {
	f := newHostedSecurityFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now()
	for _, test := range []struct {
		route, event, actor, effective, reason string
		status                                 int
	}{
		{"GET /items", "action", "actor", "actor", "", 200},
		{"GET /items", "action", "actor", "actor", "", 403},
		{"GET /items", "key_call", "actor", "actor", "", 500},
		{"GET /items", "action", "support", "actor", "support access", 200},
		{"GET /items", "session_started", "actor", "actor", "", 200},
		{"POST /items", "action", "actor", "actor", "", 200},
	} {
		seedHostedAuditRecord(t, f.service, accessstore.Audit{OrganizationID: f.service.accessOrganization(), SessionID: test.event, ActualActor: test.actor, EffectiveUser: test.effective, Reason: test.reason, Event: test.event, Route: test.route, Status: test.status, StartedAt: now, ExpiresAt: now.Add(time.Hour), RecordedAt: now, RecordedAtNS: now.UnixNano()})
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		for range commandReceiptPruneBatch + 7 {
			if _, err := w.RecordAccessAudit(t.Context(), accessstore.Audit{OrganizationID: f.service.accessOrganization(), SessionID: "legacy", ActualActor: "actor", EffectiveUser: "actor", Event: "action", Route: "GET /poll", Status: 200, StartedAt: now, ExpiresAt: now.Add(time.Hour), RecordedAt: now, RecordedAtNS: now.UnixNano()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for pass, want := range []int{8, 0, 0} {
		if err := f.service.expireNativeRecords(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		var reads, kept int
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND route LIKE 'GET %' AND status<400 AND actual_actor=effective_user AND reason='' AND event='action'`, f.service.accessOrganization()).Scan(&reads); err != nil {
			t.Fatal(err)
		}
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND session_id<>'legacy' AND NOT(event='action' AND status=200 AND actual_actor=effective_user AND route LIKE 'GET %')`, f.service.accessOrganization()).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if reads != want || kept != 5 {
			t.Fatalf("pass %d remaining reads=%d want=%d security rows=%d", pass, reads, want, kept)
		}
	}
}

func seedHostedAuditRecord(t *testing.T, service *Service, row accessstore.Audit) int64 {
	t.Helper()
	var id int64
	if err := service.config.TenantRepository.WriteTenant(t.Context(), row.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		id, err = w.RecordAccessAudit(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
