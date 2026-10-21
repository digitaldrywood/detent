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
	t.Run("portable cutoffs across organizations", func(t *testing.T) {
		f := newDefaultNativeFixture(t, Config{})
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organizations(id,name,created_at) VALUES('org_other_time','Other organization','2026-10-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

		organizations := []string{string(f.project.OrganizationID), "org_other_time"}
		before := make(map[string]string)
		for _, phase := range []string{"legacy", "populated", "repeated"} {
			if phase != "repeated" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM hosted_audit; DELETE FROM time_index_progress WHERE owner='audit'`); err != nil {
					t.Fatal(err)
				}
				for _, org := range organizations {
					for _, test := range []struct{ name, at string }{
						{"rollover", "2026-09-21T11:59:59.9996Z"},
						{"last nanosecond", "2026-09-21T11:59:59.999999999Z"},
						{"equal", "2026-09-21T12:00:00Z"},
						{"offset equal", "2026-09-21T07:00:00-05:00"},
						{"retained nanosecond", "2026-09-21T12:00:00.000000001Z"},
					} {
						var ns *int64
						if phase == "populated" {
							at, err := parseTimeValue(test.at)
							if err != nil {
								t.Fatal(err)
							}
							value := at.UnixNano()
							ns = &value
						}
						if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,recorded_at_ns) VALUES(?,'session','actor','actor','',?,'','',200,?,?,?,?)`, org, test.name, formatHubTime(now), formatHubTime(now), test.at, ns); err != nil {
							t.Fatal(err)
						}
					}
				}
				var rows, typed int
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM hosted_audit),(SELECT count(*) FROM hosted_audit_times)`).Scan(&rows, &typed); err != nil {
					t.Fatal(err)
				}
				wantTyped := 0
				if phase == "populated" {
					wantTyped = 10
				}
				if rows != 10 || typed != wantTyped {
					t.Fatalf("%s input rows=%d typed=%d want 10/%d", phase, rows, typed, wantTyped)
				}
				if phase == "populated" {
					if err := f.service.database.populateTimeIndexes(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			for _, org := range organizations {
				var events string
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT group_concat(event,',') FROM (SELECT event FROM hosted_audit WHERE organization_id=? ORDER BY id)`, org).Scan(&events); err != nil {
					t.Fatal(err)
				}
				if events != "equal,offset equal,retained nanosecond" {
					t.Fatalf("%s %s retained %s", phase, org, events)
				}
				if phase == "legacy" {
					before[org] = events
				} else if events != before[org] {
					t.Fatalf("%s %s changed retention: before=%s after=%s", phase, org, before[org], events)
				}
				var text string
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT recorded_at FROM hosted_audit WHERE organization_id=? AND event='offset equal'`, org).Scan(&text); err != nil || text != "2026-09-21T07:00:00-05:00" {
					t.Fatalf("timestamp text changed: %s err=%v", text, err)
				}
			}
		}

		var dangling int
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit_times t LEFT JOIN hosted_audit a ON a.id=t.audit_id WHERE a.id IS NULL`).Scan(&dangling); err != nil || dangling != 0 {
			t.Fatalf("dangling indexes=%d err=%v", dangling, err)
		}
	})

	t.Parallel()
	f := newHostedSecurityFixture(t)
	now := time.Date(2026, 10, 7, 22, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	insert := func(event string, at time.Time) {
		t.Helper()
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,recorded_at_ns) VALUES('org_security','session','actor','actor','',?,'','',200,?,?,?,?)`, event, formatHubTime(now), formatHubTime(now.Add(time.Hour)), formatHubTime(at), at.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	readEvents := func() string {
		t.Helper()
		var events string
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT coalesce(group_concat(event, ','),'') FROM (SELECT event FROM hosted_audit WHERE event NOT LIKE 'backlog%' ORDER BY id)").Scan(&events); err != nil {
			t.Fatal(err)
		}
		return events
	}
	for _, test := range []struct {
		event string
		at    time.Time
	}{
		{event: "31 days", at: now.Add(-31 * day)},
		{event: "just expired", at: now.Add(-30*day - time.Second)},
		{event: "30 days", at: now.Add(-30 * day)},
		{event: "29 days", at: now.Add(-29 * day)},
		{event: "recent", at: now},
	} {
		insert(test.event, test.at)
	}
	backlog := commandReceiptPruneBatch + 7
	if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,recorded_at_ns)
SELECT 'org_security','session','actor','actor','','backlog '||i,'','',200,?,?,?,? FROM n`, backlog, formatHubTime(now), formatHubTime(now.Add(time.Hour)), formatHubTime(now.Add(-45*day)), now.Add(-45*day).UnixNano()); err != nil {
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
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE event LIKE 'backlog%'").Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if events := readEvents(); remaining != want.backlog || events != want.events {
			t.Fatalf("retained events=%s backlog=%d want %+v", events, remaining, want)
		}
	}
}
