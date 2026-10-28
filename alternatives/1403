package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedImportTypedReads(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	d := f.service.database
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := d.db.ExecContext(t.Context(), statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("artifact consumption preserves unknown and typed amounts", func(t *testing.T) {
		exec(`INSERT INTO hosted_artifact_usage(singleton,service_id,usage_json,observed_at,retained_bytes,reserved_bytes) VALUES(1,'service','{"retained_bytes":11,"reserved_bytes":22}',0,101,202)`)
		for _, entry := range []struct {
			retained, reserved         any
			wantRetained, wantReserved int64
		}{
			{101, 202, 101, 202},
			{nil, 202, 11, 202},
			{101, nil, 101, 22},
			{nil, nil, 11, 22},
		} {
			exec(`UPDATE hosted_artifact_usage SET retained_bytes=?,reserved_bytes=?`, entry.retained, entry.reserved)
			retained, reserved, err := d.readHostedArtifactUsage(t.Context(), d.reader)
			if err != nil || retained != entry.wantRetained || reserved != entry.wantReserved {
				t.Fatalf("usage=%d/%d want=%d/%d error=%v", retained, reserved, entry.wantRetained, entry.wantReserved, err)
			}
		}
	})
	t.Run("session revocation honors typed subjects and evaluates unknowns", func(t *testing.T) {
		cases := []struct {
			key, payload string
			subject      any
			want         bool
		}{
			{"typed-target", "other", "target", true},
			{"typed-other", "target", "other", false},
			{"legacy-target", "target", nil, true},
			{"legacy-other", "other", nil, false},
		}
		for _, entry := range cases {
			exec(`INSERT INTO hosted_sessions(token_hash,email,identity_json,expires_at,created_at,identity_subject) VALUES(?,'user@example.test',?,?,?,?)`, entry.key, `{"subject":"`+entry.payload+`"}`, testTimestamp, testTimestamp, entry.subject)
		}
		if err := f.service.revokeHostedMemberLocally(t.Context(), "target"); err != nil {
			t.Fatal(err)
		}
		for _, entry := range cases {
			var revoked bool
			if err := d.reader.QueryRowContext(t.Context(), `SELECT revoked_at IS NOT NULL FROM hosted_sessions WHERE token_hash=?`, entry.key).Scan(&revoked); err != nil || revoked != entry.want {
				t.Fatalf("%s revoked=%v want=%v error=%v", entry.key, revoked, entry.want, err)
			}
		}
	})
	t.Run("account retry metadata is authoritative and unknown receipts remain visible", func(t *testing.T) {
		for _, entry := range []struct {
			retry          string
			kind, identity any
			lookup         string
		}{
			{"payload-identity", "account_administration", "typed-identity", "typed-identity"},
			{"legacy-identity", nil, nil, "legacy-identity"},
			{"partial-identity", "account_administration", nil, "partial-identity"},
		} {
			raw, err := json.Marshal(accountReceipt{Kind: "account_administration", Retry: entry.retry, Outcome: "pending"})
			if err != nil {
				t.Fatal(err)
			}
			exec(`INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,account_kind,account_retry) VALUES(?,'session','actor','actor','',?,'mcp/account','',0,?,?,?,?,?)`, f.project.OrganizationID, string(raw), testTimestamp, testTimestamp, testTimestamp, entry.kind, entry.identity)
			got, err := readAccountReceipt(t.Context(), d.reader, string(f.project.OrganizationID), "actor", entry.lookup)
			if err != nil || got != string(raw) {
				t.Fatalf("retry=%s receipt=%q error=%v", entry.lookup, got, err)
			}
		}
		if _, err := readAccountReceipt(t.Context(), d.reader, string(f.project.OrganizationID), "foreign-actor", "legacy-identity"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("foreign actor receipt: %v", err)
		}
	})
	t.Run("duplicate detection uses typed provenance and refuses stale legacy evaluation", func(t *testing.T) {
		for index := range 2 {
			issue := f.create(t, fmt.Sprintf("source-%d", index))
			external := fmt.Sprintf("source-node-%d", index)
			exec(`UPDATE issues SET provenance_provider='github',provenance_external_id=?,provenance_json='{"provider":"other","external_id":"payload-node"}' WHERE native_id=?`, external, issue.WorkItemID)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			preview := tracker.GitHubIssuePreview{ID: external, Number: index + 100}
			id, err := findExistingGitHubIssue(t.Context(), d.reader, scope, 0, preview)
			if err != nil || id != string(issue.WorkItemID) {
				t.Fatalf("typed duplicate=%s error=%v", id, err)
			}
			exec(`UPDATE issues SET provenance_provider=NULL,provenance_external_id=NULL,provenance_json=? WHERE native_id=?`, `{"provider":"github","external_id":"`+external+`"}`, issue.WorkItemID)
			ctx, err := prepareHostedImportFields(t.Context(), d.reader, "issues", `project_id=?`, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			exec(`UPDATE issues SET provenance_json=? WHERE native_id=?`, fmt.Sprintf(`{"provider":"github","external_id":"changed-%d"}`, index), issue.WorkItemID)
			tx, err := d.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, lookupErr := findExistingGitHubIssue(ctx, tx, scope, 0, preview)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if lookupErr == nil || errors.Is(lookupErr, sql.ErrNoRows) {
				t.Fatalf("stale snapshot hid an unevaluated identity: %v", lookupErr)
			}
		}
	})
}

func TestHostedImportOutboxFields(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	d := f.service.database
	repo, err := d.db.ExecContext(t.Context(), `INSERT INTO repositories(github_node_id,github_owner,github_name,created_at,updated_at) VALUES('outbox-repository','example-org','example-project',?,?)`, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := repo.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	items := []tracker.NativeIssue{f.create(t, "first outbox owner"), f.create(t, "second outbox owner")}
	cases := []struct {
		name, raw       string
		number, summary any
		want            string
		claim           int
	}{
		{"typed keep", `{"issue_number":99,"summary":false}`, 77, true, "pending", 77},
		{"typed number refusal", `{"issue_number":99,"summary":true}`, 0, true, "superseded", 0},
		{"typed summary refusal", `{"issue_number":99,"summary":true}`, 99, false, "superseded", 0},
		{"unknown keep", `{"issue_number":88,"summary":true}`, nil, nil, "pending", 88},
		{"unknown summary refusal", `{"issue_number":99}`, nil, nil, "superseded", 0},
		{"partial refusal", `{"issue_number":99,"summary":true}`, 0, nil, "superseded", 0},
	}
	for index, entry := range cases {
		_, err := d.db.ExecContext(t.Context(), `INSERT INTO github_outbox(idempotency_key,repository_id,issue_id,mutation_kind,desired_json,status,created_at,updated_at,desired_issue_number,desired_summary) VALUES(?,?,(SELECT id FROM issues WHERE native_id=?),'workpad',?,'pending',?,?,?,?)`, entry.name, repository, items[index%2].WorkItemID, entry.raw, testTimestamp, testTimestamp, entry.number, entry.summary)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.db.ExecContext(t.Context(), `UPDATE projects SET github_projection='disabled' WHERE id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	ctx, err := f.service.prepareHostedImportMutation(t.Context(), scope, "", updateProjectIntegrationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := supersedeDisallowedOutbox(ctx, tx, scope, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := d.commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	for _, entry := range cases {
		var status string
		if err := d.reader.QueryRowContext(t.Context(), `SELECT status FROM github_outbox WHERE idempotency_key=?`, entry.name).Scan(&status); err != nil || status != entry.want {
			t.Fatalf("%s status=%s want=%s error=%v", entry.name, status, entry.want, err)
		}
		if entry.want == "pending" {
			item, found, err := f.service.claimOutbox(t.Context())
			if err != nil || !found || item.IdempotencyKey != entry.name || item.IssueNumber != entry.claim {
				t.Fatalf("claim=%+v found=%v error=%v", item, found, err)
			}
		}
	}
}

func TestHostedImportReplaySurvivesLegacyEvaluationFailure(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	command := tracker.Mutation{IdempotencyKey: "typed-replay"}
	options := nativeCommandOptions{OperationID: "typed.preflight.replay"}
	input := CutoverRequest{}
	calls := 0
	operation := func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) { calls++; return "recorded", nil }
	first, err := f.service.executeNativeMutation(t.Context(), scope, options, command, input, operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO github_imports(id,project_id,issue_number,observed_at) VALUES('legacy-malformed',?,1,?)`, scope.project, testTimestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO github_import_records(import_id,source_key,kind,record_json,observed_at) VALUES('legacy-malformed','dependency','dependency','{"dependency_id":42}',?)`, testTimestamp); err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.executeNativeMutation(t.Context(), scope, options, command, input, operation)
	if err != nil || !bytes.Equal(first, replay) || calls != 1 {
		t.Fatalf("receipt replay=%s calls=%d error=%v", replay, calls, err)
	}
	command.IdempotencyKey = "typed-new-command"
	if _, err := f.service.executeNativeMutation(t.Context(), scope, options, command, input, operation); err == nil || calls != 1 {
		t.Fatalf("unevaluated new mutation ran: calls=%d error=%v", calls, err)
	}
}
