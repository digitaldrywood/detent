package runnerfleet_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerRepositoryReports(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewNative(t, nil, "", "repositories")
	other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "other")
	first := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	second := hubfixture.PrepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	first.Enroll(t)
	second.Enroll(t)
	denied := f.Hub.Request(t, http.MethodPost, f.Base+"/onboarding/repositories", f.Token, map[string]any{"runner_id": first.Binding.RunnerID, "idempotency_key": "denied"})
	hubfixture.RequireStatus(t, denied, http.StatusForbidden)
	var refusal struct {
		Code string `json:"code"`
	}
	hubfixture.Decode(t, denied, &refusal)
	if refusal.Code != "access_denied" {
		t.Fatalf("visible non-administrator refusal code=%q, want access_denied", refusal.Code)
	}
	ctx := t.Context()
	read := func(runner string) (*runnerauth.RepositoryList, error) {
		list := &runnerauth.RepositoryList{Repositories: []runnerauth.Repository{}}
		err := f.Hub.Config.TenantRepository.ReadTenant(ctx, f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.RunnerRepositoryList(ctx, runner)
			if err != nil {
				return err
			}
			list.RequestID, list.Pending, list.Reason = row.RequestID, row.Pending, row.Reason
			if row.ListedAt != nil {
				list.ListedAt = hubfixture.FormatTime(*row.ListedAt)
			}
			repositories, err := r.RunnerRepositories(ctx, runner, 200)
			if err != nil {
				return err
			}
			if len(repositories) == 200 {
				return errors.New("repository fixture exceeded the read page")
			}
			for _, repository := range repositories {
				list.Repositories = append(list.Repositories, runnerauth.Repository{Owner: repository.Owner, Name: repository.Name, Private: repository.Private, PushedAt: repository.PushedAt})
			}
			return nil
		})
		return list, err
	}
	request := func(base, runner, key string) string {
		t.Helper()
		response := f.Hub.Request(t, http.MethodPost, base+"/onboarding/repositories", hubfixture.AdminToken, map[string]any{"runner_id": runner, "idempotency_key": key})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var result struct {
			RequestID string `json:"request_id"`
		}
		hubfixture.Decode(t, response, &result)
		return result.RequestID
	}
	for i, test := range []struct{ base, id, credential, machine string }{
		{f.Base, first.Binding.RunnerID, first.Redemption.Credential, string(first.Binding.MachineID)},
		{other.Base, second.Binding.RunnerID, second.Redemption.Credential, string(second.Binding.MachineID)},
	} {
		requestID := request(test.base, test.id, "request")
		if request(test.base, test.id, "request") != requestID {
			t.Fatal("refresh replay changed request")
		}
		var snapshot runnerauth.RoutingSnapshot
		response := f.Hub.Request(t, http.MethodPost, test.base+"/machines/"+test.machine+"/heartbeat", test.credential, map[string]any{"display_name": "Runner", "capacity": 2, "version": "test"})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		hubfixture.Decode(t, response, &snapshot)
		if snapshot.RepositoryListRequest != requestID {
			t.Fatalf("command=%+v", snapshot)
		}
		setupList := func() *runnerauth.RepositoryList {
			t.Helper()
			response := f.Hub.Request(t, http.MethodGet, test.base+"/onboarding", hubfixture.AdminToken, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			hubfixture.Decode(t, response, &setup)
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
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body), http.StatusOK)
		if list := setupList(); list.Pending || list.RequestID != requestID || len(list.Repositories) != 2 || list.Repositories[0].Owner != "example-org" || list.Repositories[0].Name != "private" {
			t.Fatalf("reported repositories missing from setup read: %+v", list)
		}
		list, err := read(test.id)
		if err != nil || list.Pending || list.ListedAt == "" || len(list.Repositories) != 2 || !list.Repositories[0].Private || list.Repositories[0].PushedAt == nil || !list.Repositories[0].PushedAt.Equal(at) {
			t.Fatalf("report=%+v err=%v", list, err)
		}
		if i == 0 {
			if _, err := f.Hub.Admin.ExecContext(ctx, `CREATE FUNCTION reject_repository_report() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'repository report rejected'; END $$; CREATE TRIGGER reject_repository BEFORE INSERT ON runner_repositories FOR EACH ROW WHEN (NEW.name='reject') EXECUTE FUNCTION reject_repository_report()`); err != nil {
				t.Fatal(err)
			}
			nextID := request(test.base, test.id, "replace")
			body["idempotency_key"] = "late-report"
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body), http.StatusUnprocessableEntity)
			body["request_id"], body["idempotency_key"] = nextID, nextID
			body["repositories"] = []runnerauth.Repository{{Owner: "example-org", Name: "new"}, {Owner: "example-org", Name: "reject"}}
			failed := f.Hub.Request(t, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body)
			hubfixture.RequireStatus(t, failed, http.StatusInternalServerError)
			list, err = read(test.id)
			if err != nil || len(list.Repositories) != 2 || list.Repositories[0].Name != "private" || !list.Pending {
				t.Fatalf("failed replacement lost list: %+v %v", list, err)
			}
			body["repositories"] = []runnerauth.Repository{{Owner: "example-org", Name: "replacement"}}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, test.base+"/onboarding/repositories/report", test.credential, body), http.StatusOK)
		}
	}
	firstList, err := read(first.Binding.RunnerID)
	if err != nil || len(firstList.Repositories) != 1 || firstList.Repositories[0].Name != "replacement" {
		t.Fatalf("first list=%+v %v", firstList, err)
	}
	secondList, err := read(second.Binding.RunnerID)
	if err != nil || len(secondList.Repositories) != 2 {
		t.Fatalf("second list=%+v %v", secondList, err)
	}
	for _, test := range []struct {
		name, token      string
		runners, visible bool
	}{{"administrator", hubfixture.AdminToken, true, true}, {"no routing rights", f.Token, true, false}, {"runner credential", first.Redemption.Credential, false, false}} {
		t.Run(test.name, func(t *testing.T) {
			response := f.Hub.Request(t, http.MethodGet, f.Base+"/onboarding", test.token, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			hubfixture.Decode(t, response, &setup)
			if !test.runners {
				if len(setup.Runners) != 0 {
					t.Fatalf("worker onboarding read exposed runners=%+v", setup.Runners)
				}
				return
			}
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
	seedStartupOrganization(t, f.Hub.Config.TenantRepository, "org_foreign")
	hubfixture.SeedAdmin(t, f.Hub.Config.TenantRepository, "org_foreign", hubfixture.AdminToken, f.Hub.Now())
	foreign := hubfixture.NewNative(t, f.Hub, "org_foreign", "foreign")
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, foreign.Base+"/onboarding/repositories", hubfixture.AdminToken, map[string]any{"runner_id": first.Binding.RunnerID, "idempotency_key": "foreign"}), http.StatusNotFound)
	emptyID := request(f.Base, first.Binding.RunnerID, "signed-out")
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/onboarding/repositories/report", first.Redemption.Credential, map[string]any{"idempotency_key": emptyID, "request_id": emptyID, "repositories": []runnerauth.Repository{}, "reason": "no GitHub CLI sign-in"}), http.StatusOK)
	empty, err := read(first.Binding.RunnerID)
	if err != nil || len(empty.Repositories) != 0 || empty.Reason != "no GitHub CLI sign-in" || empty.Pending {
		t.Fatalf("signed-out replacement=%+v %v", empty, err)
	}
}
