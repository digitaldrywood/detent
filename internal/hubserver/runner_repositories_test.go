package hubserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerRepositoryReports(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "repositories")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	second := prepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	first.enroll(t)
	second.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/repositories", f.token, map[string]any{"runner_id": first.binding.RunnerID, "idempotency_key": "denied"}), http.StatusNotFound)
	ctx := t.Context()
	request := func(base, runner, key string) string {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/onboarding/repositories", testHubAdminToken, map[string]any{"runner_id": runner, "idempotency_key": key})
		requireNativeStatus(t, response, http.StatusOK)
		var result struct {
			RequestID string `json:"request_id"`
		}
		decodeHubResponse(t, response, &result)
		return result.RequestID
	}
	for i, test := range []struct{ base, id, credential, machine string }{
		{f.base, first.binding.RunnerID, first.redemption.Credential, string(first.binding.MachineID)},
		{other.base, second.binding.RunnerID, second.redemption.Credential, string(second.binding.MachineID)},
	} {
		requestID := request(test.base, test.id, "request")
		if request(test.base, test.id, "request") != requestID {
			t.Fatal("refresh replay changed request")
		}
		var snapshot runnerauth.RoutingSnapshot
		response := performHubAPIRequest(t, f.service, http.MethodPost, test.base+"/machines/"+test.machine+"/heartbeat", test.credential, map[string]any{"capacity": 2, "version": "test"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &snapshot)
		if snapshot.RepositoryListRequest != requestID {
			t.Fatalf("command=%+v", snapshot)
		}
		setupList := func() *runnerauth.RepositoryList {
			t.Helper()
			response := performHubAPIRequest(t, f.service, http.MethodGet, test.base+"/onboarding", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			for _, runner := range setup.Runners {
				if runner.Runner.RunnerID == test.id && runner.RepositoryList != nil {
					return runner.RepositoryList
				}
			}
			t.Fatal("runner repository list missing from setup read")
			return nil
		}
		if list := setupList(); !list.Pending || list.RequestID != requestID {
			t.Fatalf("pending setup list=%+v", list)
		}
		at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		body := map[string]any{"idempotency_key": requestID, "request_id": requestID, "repositories": []runnerauth.Repository{{Owner: "example-org", Name: "private", Private: true, PushedAt: &at}, {Owner: "example-user", Name: "public"}}}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body), http.StatusOK)
		if list := setupList(); list.Pending || list.RequestID != requestID || len(list.Repositories) != 2 || list.Repositories[0].Owner != "example-org" || list.Repositories[0].Name != "private" {
			t.Fatalf("reported repositories missing from setup read: %+v", list)
		}
		list, err := readRunnerRepositories(ctx, f.service.database.reader, test.id)
		if err != nil || list.Pending || list.ListedAt == "" || len(list.Repositories) != 2 || !list.Repositories[0].Private || list.Repositories[0].PushedAt == nil || !list.Repositories[0].PushedAt.Equal(at) {
			t.Fatalf("report=%+v err=%v", list, err)
		}
		if i == 0 {
			if _, err := f.service.database.db.ExecContext(ctx, `CREATE TRIGGER reject_repository BEFORE INSERT ON runner_repositories WHEN NEW.name='reject' BEGIN SELECT RAISE(ABORT,'repository report rejected'); END`); err != nil {
				t.Fatal(err)
			}
			nextID := request(test.base, test.id, "replace")
			body["idempotency_key"] = "late-report"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body), http.StatusUnprocessableEntity)
			body["request_id"], body["idempotency_key"] = nextID, nextID
			body["repositories"] = []runnerauth.Repository{{Owner: "example-org", Name: "new"}, {Owner: "example-org", Name: "reject"}}
			failed := performHubAPIRequest(t, f.service, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body)
			requireNativeStatus(t, failed, http.StatusInternalServerError)
			list, err = readRunnerRepositories(ctx, f.service.database.reader, test.id)
			if err != nil || len(list.Repositories) != 2 || list.Repositories[0].Name != "private" || !list.Pending {
				t.Fatalf("failed replacement lost list: %+v %v", list, err)
			}
			body["repositories"] = []runnerauth.Repository{{Owner: "example-org", Name: "replacement"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body), http.StatusOK)
		}
	}
	firstList, err := readRunnerRepositories(ctx, f.service.database.reader, first.binding.RunnerID)
	if err != nil || len(firstList.Repositories) != 1 || firstList.Repositories[0].Name != "replacement" {
		t.Fatalf("first list=%+v %v", firstList, err)
	}
	secondList, err := readRunnerRepositories(ctx, f.service.database.reader, second.binding.RunnerID)
	if err != nil || len(secondList.Repositories) != 2 {
		t.Fatalf("second list=%+v %v", secondList, err)
	}
	for _, test := range []struct {
		name, token string
		visible     bool
	}{{"administrator", testHubAdminToken, true}, {"no routing rights", f.token, false}, {"runner credential", first.redemption.Credential, false}} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", test.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			if len(setup.Runners) == 0 {
				t.Fatal("missing runner fixture")
			}
			for _, runner := range setup.Runners {
				if (runner.RepositoryList != nil) != test.visible {
					t.Fatalf("repository visibility=%+v", runner.RepositoryList)
				}
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(ctx, "INSERT INTO organizations(id,name,local,created_at) VALUES('org_foreign','Example organization',0,?)", formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
	foreign := newNativeFixture(t, f.service, "org_foreign", "foreign")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreign.base+"/onboarding/repositories", testHubAdminToken, map[string]any{"runner_id": first.binding.RunnerID, "idempotency_key": "foreign"}), http.StatusNotFound)
	emptyID := request(f.base, first.binding.RunnerID, "signed-out")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/repositories/report", first.redemption.Credential, map[string]any{"idempotency_key": emptyID, "request_id": emptyID, "repositories": []runnerauth.Repository{}, "reason": "no GitHub CLI sign-in"}), http.StatusOK)
	empty, err := readRunnerRepositories(ctx, f.service.database.reader, first.binding.RunnerID)
	if err != nil || len(empty.Repositories) != 0 || empty.Reason != "no GitHub CLI sign-in" || empty.Pending {
		t.Fatalf("signed-out replacement=%+v %v", empty, err)
	}
}
