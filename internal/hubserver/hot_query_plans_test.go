package hubserver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestHotQueryPlans(t *testing.T) {
	f := newChangeFixture(t, nil)
	f.publish(t, "current", "")
	other := f.create(t, "related")
	db := f.service.database.db
	for n := range 64 {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)", fmt.Sprintf("org_plan_%d", n), "Example", formatHubTime(f.service.config.now())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), "ANALYZE organizations"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `WITH RECURSIVE copies(n) AS (SELECT 3 UNION ALL SELECT n+1 FROM copies WHERE n<256)
 INSERT INTO issues(native_id,organization_id,project_id,number,workflow_state_id,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at)
 SELECT 'wi_plan_'||n,organization_id,project_id,n,workflow_state_id,'Example','','open','1',source_updated_at,synchronized_at,created_at,updated_at FROM issues,copies WHERE native_id=?`, other.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO issue_relationships SELECT a.id,b.id,'related',a.created_at FROM issues a JOIN issues b ON b.id=a.id+1 WHERE a.native_id LIKE 'wi_plan_%' AND b.native_id LIKE 'wi_plan_%'`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(t.Context(), `INSERT INTO issue_relationships SELECT a.id,b.id,'related',a.native_created_at FROM issues a,issues b WHERE a.native_id=? AND b.native_id=?`, f.issue.WorkItemID, other.WorkItemID); err != nil {
		t.Fatal(err)
	}
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim)
	r.enroll(t)
	q := &runtimeReadQuery{nativeQueryer: db}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: r.binding.RunnerID, Hash: apikey.HashToken(r.redemption.Credential), Scope: apiScopeWorker, Runner: r.identity}}
	if err := readRunnerAuthority(t.Context(), q, scope, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if len(q.statements) != 1 || q.statements[0] != runnerAuthoritySelect {
		t.Fatalf("runner authority hydrated unrelated state: %v", q.statements)
	}

	if _, err := readNativeRelated(t.Context(), q, scope, f.issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := nativeCommandReceiptReference(t.Context(), q, scope, "POST /example", "missing", "hash"); err != nil || found {
		t.Fatalf("missing command receipt found=%t: %v", found, err)
	}
	for i, statement := range q.statements {
		plans := explainQueryPlan(t, db, statement, q.arguments[i])
		t.Logf("SQL %d: %s\nPLAN:\n%s", i, statement, strings.Join(plans, "\n"))
		assertHotQueryPlan(t, statement, plans)
	}
}

func assertHotQueryPlan(t *testing.T, statement string, plans []string) {
	t.Helper()
	var lookups map[string][]string
	switch {
	case strings.Contains(statement, "FROM native_commands WHERE organization_id"):
		lookups = map[string][]string{"native_commands": {"organization_id=?", "actor_id=?", "command_key=?", "operation>?", "operation<?"}}
	case strings.Contains(statement, "CROSS JOIN issue_relationships"):
		lookups = map[string][]string{"source": {"native_id=?"}, "r": {"first_issue_id=?", "second_issue_id=?"}}
	case statement == runnerAuthoritySelect:
		lookups = map[string][]string{"r": {"id=?|token_id=?"}, "t": {"id=?"}, "g": {"token_id=?"}}
	default:
		t.Fatalf("unrecognized query: %s", statement)
	}
	for table, keys := range lookups {
		for _, detail := range plans {
			if detail == "SCAN "+table || strings.HasPrefix(detail, "SCAN "+table+" ") {
				t.Fatalf("full scan of %s: %v", table, plans)
			}
		}
		for _, key := range keys {
			found := false
			for _, detail := range plans {
				if strings.HasPrefix(detail, "SEARCH "+table+" USING ") && strings.Contains(detail, "INDEX ") {
					for _, alternative := range strings.Split(key, "|") {
						found = found || strings.Contains(detail, "("+alternative) || strings.Contains(detail, "AND "+alternative)
					}
				}
			}
			if !found {
				t.Fatalf("missing indexed lookup of %s on %s: %v", table, key, plans)
			}
		}
	}
}
