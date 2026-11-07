package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCancellationPublicationWireBudget(t *testing.T) {
	first, second, _, wire, logs := newRunnerEventWireFixtures(t)
	if _, err := first.admin.ExecContext(t.Context(), `CREATE FUNCTION reject_runner_release() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF OLD.release_reason='fixture_reject' AND NEW.released_at IS NOT NULL THEN RAISE EXCEPTION 'fixture release rejection'; END IF; RETURN NEW; END$$; CREATE TRIGGER reject_runner_release BEFORE UPDATE ON leases FOR EACH ROW EXECUTE FUNCTION reject_runner_release()`); err != nil {
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
			path := fmt.Sprintf("/api/v2/organizations/%s/projects/prj_runner/leases/%s/release", org, lease.ID)
			request := tracker.NativeLeaseMutation{FencingToken: lease.FencingToken + 1, Reason: "cancelled", CancelAttempt: true}
			requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path, fixture.tokens[index], request), http.StatusConflict)
			request.FencingToken = lease.FencingToken
			if _, err := fixture.admin.ExecContext(t.Context(), `UPDATE leases SET release_reason='fixture_reject' WHERE organization_id=$1 AND lease_id=$2`, org, lease.ID); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, fixture.service, http.MethodPost, path, fixture.tokens[index], request)
			if response.Code < 500 {
				t.Fatalf("release rejection status=%d body=%s diagnostics=%s", response.Code, response.Body.String(), logs.String())
			}
			var sequence, events int64
			var status string
			var released bool
			read := func() {
				t.Helper()
				if err := fixture.admin.QueryRowContext(t.Context(), `SELECT a.sequence,a.status,l.released_at IS NOT NULL,(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND attempt_id=$2 AND type='run.finished') FROM native_attempts a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id WHERE a.organization_id=$1 AND a.id=$2`, org, data.AttemptID).Scan(&sequence, &status, &released, &events); err != nil {
					t.Fatal(err)
				}
			}
			read()
			if sequence != 1 || status != "running" || released || events != 0 {
				t.Fatalf("failed cancellation leaked: sequence=%d status=%s released=%t events=%d", sequence, status, released, events)
			}
			if _, err := fixture.admin.ExecContext(t.Context(), `UPDATE leases SET release_reason='' WHERE organization_id=$1 AND lease_id=$2`, org, lease.ID); err != nil {
				t.Fatal(err)
			}
			wire.Lock()
			wire.statements, wire.exchanges, wire.transactions = 0, 0, 0
			wire.Unlock()
			response = httptest.NewRecorder()
			ctx := context.WithValue(t.Context(), claimRequestTraceKey{}, true)
			fixture.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path, fixture.tokens[index], request).WithContext(ctx))
			if response.Code != http.StatusNoContent {
				t.Log(logs.String())
			}
			requireNativeStatus(t, response, http.StatusNoContent)
			wire.Lock()
			actual := [2]int{wire.statements, wire.exchanges}
			wire.Unlock()
			t.Logf("tenant=%s history=%d cancellation HTTP statements=%d exchanges=%d", org, history, actual[0], actual[1])
			if actual[1] < 1 || actual[1] > 8 || expected != [2]int{} && actual != expected {
				t.Errorf("cancellation bound=%v previous=%v want <=8 exchanges, fixed across history and tenants", actual, expected)
			}
			expected = actual
			read()
			if sequence != 2 || status != "cancelled" || !released || events != 1 {
				t.Fatalf("cancellation missing: sequence=%d status=%s released=%t events=%d", sequence, status, released, events)
			}
		}
	}
}
