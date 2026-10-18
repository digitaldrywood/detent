package hubserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/mutation"
)

func TestHostedAuditSecurityEvents(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "owner", "owner", "owner@example.test", "write", "support@example.test")
	for _, test := range []struct {
		name, event, route string
		mutation, keep     bool
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
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			if test.mutation {
				ctx = mutation.WithContext(ctx, mutation.Metadata{Action: "update", Source: "mcp", CorrelationID: "correlation", RetryIdentity: "private-retry", InputHash: "private-input"})
			}
			if err := f.service.hostedAudit(ctx, user.identity.Hosted, test.event, test.route, string(f.project), http.StatusOK); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE event=? AND route=?", test.event, test.route).Scan(&count); err != nil {
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
				var actor, effective, action string
				var retry, input any
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT actual_actor,effective_user,json_extract(mutation_json,'$.action'),json_extract(mutation_json,'$.retry_identity'),json_extract(mutation_json,'$.input_hash') FROM hosted_audit WHERE event=? AND route=?", test.event, test.route).Scan(&actor, &effective, &action, &retry, &input); err != nil {
					t.Fatal(err)
				}
				if actor != "support@example.test" || effective != user.identity.Subject || action != "update" || retry != nil || input != nil {
					t.Fatalf("mutation audit actor=%s effective=%s action=%s retry=%v input=%v", actor, effective, action, retry, input)
				}
			}
		})
	}
}

func TestHostedAuditRetention(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	now := time.Date(2026, 10, 7, 22, 0, 0, 0, time.UTC)
	cutoff := now.Add(-90 * 24 * time.Hour)
	for _, test := range []struct {
		event string
		at    time.Time
	}{
		{event: "expired", at: cutoff.Add(-time.Second)},
		{event: "boundary", at: cutoff},
		{event: "newer", at: cutoff.Add(time.Second)},
		{event: "recent", at: now},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at) VALUES('org_security','session','actor','actor','',?,'','',200,?,?,?)`, test.event, formatHubTime(now), formatHubTime(now.Add(time.Hour)), formatHubTime(test.at)); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		var events string
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT group_concat(event, ',') FROM (SELECT event FROM hosted_audit ORDER BY id)").Scan(&events); err != nil {
			t.Fatal(err)
		}
		if events != "boundary,newer,recent" {
			t.Fatalf("retained events=%s", events)
		}
	}
}
