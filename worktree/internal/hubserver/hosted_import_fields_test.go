package hubserver

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestHostedImportFieldBackfill(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	d := f.service.database
	if err := d.populateHostedImportFields(t.Context()); err != nil {
		t.Fatal(err)
	}
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := d.db.ExecContext(t.Context(), statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := d.db.ExecContext(t.Context(), `INSERT INTO repositories(github_node_id,github_owner,github_name,created_at,updated_at) VALUES('typed-repository','example-org','example-project',?,?)`, testTimestamp, testTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := repo.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var checks []struct {
		query, want string
		args        []any
	}
	check := func(query, want string, args ...any) {
		checks = append(checks, struct {
			query, want string
			args        []any
		}{query, want, args})
	}
	for index := range 2 {
		key := fmt.Sprintf("typed-%d", index)
		fixture := f
		if index > 0 {
			fixture = newNativeFixture(t, f.service, "", "second-typed-project")
		}
		item := fixture.create(t, key)
		exec(`UPDATE issues SET provenance_json=?,provenance_provider=NULL,provenance_external_id=NULL WHERE native_id=?`, `{"provider":"github","external_id":"`+key+`"}`, item.WorkItemID)
		check(`SELECT provenance_provider||'|'||provenance_external_id FROM issues WHERE native_id=?`, "github|"+key, item.WorkItemID)
		exec(`INSERT INTO hosted_plans(id,version,record_json) VALUES(?,1,'{"allowances":{"collaboration_bytes":123}}')`, key)
		check(`SELECT CAST(collaboration_bytes AS TEXT) FROM hosted_plans WHERE id=?`, "123", key)
		exec(`INSERT INTO hosted_sessions(token_hash,email,identity_json,expires_at,created_at) VALUES(?,'user@example.test',?,?,?)`, key, `{"subject":"`+key+`"}`, testTimestamp, testTimestamp)
		check(`SELECT identity_subject FROM hosted_sessions WHERE token_hash=?`, key, key)
		exec(`INSERT INTO artifact_services(organization_id,project_id,id,binding_json,publisher_token_id) VALUES(?,?,?,'{"mode":"hosted","hosted_opt_in":true}',?)`, f.project.OrganizationID, f.project.ID, key, defaultHubOperatorID)
		check(`SELECT binding_mode||'|'||hosted_opt_in FROM artifact_services WHERE id=?`, "hosted|1", key)
		observation := costTestObservation("cpu", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC))
		observation.SourceID, observation.ResourceID, observation.Voided = key, key, index == 0
		if _, err := recordTestCost(t, d.db, string(f.project.OrganizationID), string(fixture.project.ID), observation, observation.To); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE usage_cost_observations SET voided=NULL WHERE source_id=?`, key)
		check(`SELECT CAST(voided AS TEXT) FROM usage_cost_observations WHERE source_id=?`, strconv.Itoa(1-index), key)
		exec(`INSERT INTO github_imports(id,project_id,issue_number,work_item_id,observed_at) VALUES(?,?,?,?,?)`, key, fixture.project.ID, index+1, item.WorkItemID, testTimestamp)
		exec(`INSERT INTO github_import_records(import_id,source_key,kind,record_json,observed_at) VALUES(?,?,'dependency',?,?)`, key, key, `{"dependency_id":"blocker","provenance":{"external_id":"`+key+`"}}`, testTimestamp)
		check(`SELECT dependency_id||'|'||provenance_external_id FROM github_import_records WHERE import_id=?`, "blocker|"+key, key)
		exec(`INSERT INTO onboarding_issue_intake(project_id,request_json) VALUES(?,'{"hub_owned":true,"status":"importing"}')`, fixture.project.ID)
		check(`SELECT hub_owned||'|'||batch_status FROM onboarding_issue_intake WHERE project_id=?`, "1|importing", fixture.project.ID)
		exec(`INSERT INTO github_outbox(idempotency_key,repository_id,mutation_kind,desired_json,status,created_at,updated_at) VALUES(?,?,'workpad','{"issue_number":42,"summary":true}','pending',?,?)`, key, repository, testTimestamp, testTimestamp)
		check(`SELECT desired_issue_number||'|'||desired_summary FROM github_outbox WHERE idempotency_key=?`, "42|1", key)
		runner := prepareRunner(t, fixture, runnerauth.Read, runnerauth.Heartbeat)
		runner.enroll(t)
		exec(`DELETE FROM runner_operations WHERE runner_id=?`, runner.binding.RunnerID)
		exec(`UPDATE runner_identities SET operations_known=NULL WHERE id=?`, runner.binding.RunnerID)
		check(`SELECT CAST(count(*) AS TEXT) FROM runner_operations WHERE runner_id=? AND operation='heartbeat'`, "1", runner.binding.RunnerID)
	}
	exec(`INSERT INTO hosted_artifact_usage(singleton,service_id,usage_json,observed_at) VALUES(1,'typed-service','{"retained_bytes":11,"reserved_bytes":22}',0)`)
	check(`SELECT retained_bytes||'|'||reserved_bytes FROM hosted_artifact_usage`, "11|22")
	for index := range hostedImportRowsPerTick + 3 {
		key := fmt.Sprintf("receipt-%d", index)
		raw, err := json.Marshal(accountReceipt{Kind: "account_administration", Retry: key})
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at) VALUES(?,'typed-session','typed-actor','typed-actor','',?,'mcp/account','',0,?,?,?)`, f.project.OrganizationID, string(raw), testTimestamp, testTimestamp, testTimestamp)
		check(`SELECT account_kind||'|'||account_retry FROM hosted_audit WHERE event=?`, "account_administration|"+key, string(raw))
	}
	unknown := func() int {
		t.Helper()
		total := 0
		for _, projection := range hostedImportProjections {
			var count int
			if err := d.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM `+projection.table+` WHERE `+projection.columns[0]+` IS NULL`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			total += count
		}
		return total
	}
	before := unknown()
	var writes int
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		if strings.HasPrefix(query, "UPDATE ") {
			writes++
		}
	}})
	if changed, err := d.populateHostedImportField(ctx); err != nil || !changed {
		t.Fatalf("first backfill: changed=%v error=%v", changed, err)
	}
	if got := unknown(); got != before-1 || writes != 1 {
		t.Fatalf("one transaction changed %d rows with %d updates", before-got, writes)
	}
	if err := d.populateHostedImportFields(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := unknown(); got != before-1-hostedImportRowsPerTick {
		t.Fatalf("tick changed %d rows, want %d", before-1-got, hostedImportRowsPerTick)
	}
	if err := d.populateHostedImportFields(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := unknown(); got != 0 {
		t.Fatalf("backfill did not finish in two ticks: %d unknown rows", got)
	}
	for _, entry := range checks {
		var got string
		if err := d.reader.QueryRowContext(t.Context(), entry.query, entry.args...).Scan(&got); err != nil || got != entry.want {
			t.Fatalf("%s: got %q, want %q, error=%v", entry.query, got, entry.want, err)
		}
	}
	if changed, err := d.populateHostedImportField(t.Context()); err != nil || changed {
		t.Fatalf("completed backfill repeated work: changed=%v error=%v", changed, err)
	}
}
