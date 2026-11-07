package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCompletionPublicationWireBudget(t *testing.T) {
	first, second, repository, wire, logs := newRunnerEventWireFixtures(t)
	if _, err := first.admin.ExecContext(t.Context(), `CREATE FUNCTION reject_completion_receipt() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.command_key='rollback_completion' THEN RAISE EXCEPTION 'fixture publication rejection'; END IF; RETURN NEW; END$$; CREATE TRIGGER reject_completion_receipt BEFORE INSERT ON native_commands FOR EACH ROW EXECUTE FUNCTION reject_completion_receipt()`); err != nil {
		t.Fatal(err)
	}
	var expected [2]int
	for tenantIndex, fixture := range []runnerPGFixture{first, second} {
		org := tracker.OrganizationID("org_startup")
		if tenantIndex == 1 {
			org = "org_second"
		}
		for index, history := range []int{2, 50} {
			lease, data := fixture.leases[index], fixture.data[index]
			seedRunnerCommandHistory(t, fixture, org, index, history)
			var attempt leasestore.Attempt
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				attempt, err = r.Attempt(t.Context(), data.AttemptID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',0) ON CONFLICT DO NOTHING;`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.admin.ExecContext(t.Context(), `INSERT INTO attachments(organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at,work_item_id,evidence_attempt_id) SELECT $1,'prj_runner',$2,id,'evidence.txt','text/plain',1,$3,clock_timestamp(),$4,$5 FROM api_tokens WHERE organization_id=$1 AND token_hash=$6`, org, fmt.Sprintf("evidence_%d", index), strings.Repeat("a", 64), lease.WorkItemID, data.AttemptID, apikey.HashToken(fixture.tokens[index])); err != nil {
				t.Fatal(err)
			}
			paid := int64(1000)
			ended := time.Now().UTC()
			data.Sequence, data.Outcome, data.CompletionBody = 2, "succeeded", "Example completion summary"
			data.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: 17, CachedInput: 2, Output: 3, BillingMode: "metered", Currency: "USD", Revision: 1, From: attempt.StartedAt, To: ended, ReportedAt: ended, ReportedCostMicros: &paid, CostSource: "runner_report", CostCoverage: "complete"}}
			finish := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "rollback_completion"}, Type: "run.finished", SchemaVersion: 1, OccurredAt: ended, Data: data}
			path := fmt.Sprintf("/api/v2/organizations/%s/projects/prj_runner/work-items/%s/events", org, lease.WorkItemID)
			response := performHubAPIRequest(t, fixture.service, http.MethodPost, path, fixture.tokens[index], finish)
			if response.Code < 500 {
				t.Fatalf("publication rejection status=%d body=%s diagnostics=%s", response.Code, response.Body.String(), logs.String())
			}
			var sequence, comments, costs, reports, search int64
			if err := fixture.admin.QueryRowContext(t.Context(), `SELECT a.sequence,(SELECT count(*) FROM native_comments WHERE organization_id=$1 AND work_item_id=$3),(SELECT count(*) FROM cost_sources WHERE organization_id=$1 AND resource_id=$2),(SELECT count(*) FROM report_usage_current WHERE organization_id=$1 AND attempt_id=$2),(SELECT count(*) FROM search_documents WHERE organization_id=$1 AND work_item_id=$3 AND kind='comment') FROM native_attempts a WHERE organization_id=$1 AND id=$2`, org, data.AttemptID, lease.WorkItemID).Scan(&sequence, &comments, &costs, &reports, &search); err != nil {
				t.Fatal(err)
			}
			if sequence != 1 || comments != 0 || costs != 0 || reports != 0 || search != 0 {
				t.Fatalf("rejected completion leaked publication: %d %d %d %d %d", sequence, comments, costs, reports, search)
			}
			finish.IdempotencyKey = "finish"
			wire.Lock()
			wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
			wire.Unlock()
			response = httptest.NewRecorder()
			ctx := context.WithValue(t.Context(), claimRequestTraceKey{}, true)
			fixture.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path, fixture.tokens[index], finish).WithContext(ctx))
			if response.Code != http.StatusOK {
				t.Log(logs.String())
			}
			requireNativeStatus(t, response, http.StatusOK)
			wire.Lock()
			actual := [2]int{wire.statements, wire.exchanges}
			wire.Unlock()
			t.Logf("tenant=%s history=%d completion HTTP statements=%d exchanges=%d", org, history, actual[0], actual[1])
			if actual[1] < 1 || actual[1] > 8 || expected != [2]int{} && actual != expected {
				t.Errorf("completion bound=%v previous=%v want <=8 exchanges, fixed across history and tenants", actual, expected)
			}
			expected = actual
			if err := fixture.admin.QueryRowContext(t.Context(), `SELECT a.sequence,(SELECT count(*) FROM native_comments WHERE organization_id=$1 AND work_item_id=$3),(SELECT count(*) FROM cost_sources WHERE organization_id=$1 AND resource_id=$2),(SELECT count(*) FROM report_usage_current WHERE organization_id=$1 AND attempt_id=$2 AND input=17),(SELECT count(*) FROM search_documents WHERE organization_id=$1 AND work_item_id=$3 AND kind='comment') FROM native_attempts a WHERE organization_id=$1 AND id=$2`, org, data.AttemptID, lease.WorkItemID).Scan(&sequence, &comments, &costs, &reports, &search); err != nil {
				t.Fatal(err)
			}
			if sequence != 2 || comments != 1 || costs != 1 || reports != 1 || search != 1 {
				t.Fatalf("completion missing publication: %d %d %d %d %d", sequence, comments, costs, reports, search)
			}
		}
	}
}
