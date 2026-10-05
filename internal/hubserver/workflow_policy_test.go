package hubserver

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestObservedRepositoryWorkflowApply(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                                                  string
		initial, reachable, local, occupied, wrongRepository, legacy, missingRevision, replay bool
		status                                                                                int
		applied                                                                               bool
	}{
		{name: "default branch initial definition", reachable: true, status: http.StatusNoContent, applied: true},
		{name: "default branch revision", initial: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "previously applied definition records each apply", initial: true, reachable: true, replay: true, status: http.StatusNoContent, applied: true},
		{name: "feature branch pending", initial: true, status: http.StatusNoContent},
		{name: "uncommitted definition pending", initial: true, local: true, reachable: true, status: http.StatusNoContent},
		{name: "occupied lane rejected", initial: true, reachable: true, occupied: true, status: http.StatusUnprocessableEntity},
		{name: "different repository pending", initial: true, reachable: true, wrongRepository: true, status: http.StatusNoContent},
		{name: "unenrolled reporter pending", initial: true, reachable: true, legacy: true, status: http.StatusNoContent},
		{name: "missing commit pending", initial: true, reachable: true, missingRevision: true, status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", test.name)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = ? WHERE id = ?", "acme/orders", f.project.ID); err != nil {
				t.Fatal(err)
			}
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			runner.enroll(t)
			original := hubTestPolicy()
			original.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("a", 40), States: nativeFixtureStates()}
			original = original.WithID()
			historyCount := 0
			if test.initial {
				approveHubTestPolicy(t, f.service, f.base+"/policy", original)
				historyCount++
			}
			item := f.create(t, "Preserve occupied Todo and revision")
			candidate := original
			candidate.SourceDigest = policy.Digest([]byte("changed repository definition"))
			candidate.SourceRevision = strings.Repeat("b", 40)
			candidate.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("b", 40), States: append(nativeFixtureStates(), policy.State{Name: "Repair", Dispatchable: true})}
			if test.occupied {
				candidate.Workflow.States = []policy.State{{Name: "Repair", Dispatchable: true}}
			}
			if test.missingRevision {
				candidate.Workflow.Revision = ""
			}
			candidate = candidate.WithID()
			source := &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("b", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("c", 40), DefaultBranchReachable: test.reachable}
			if test.local {
				source = nil
			}
			if test.wrongRepository {
				source.Repository = "acme/other"
			}
			token := runner.redemption.Credential
			if test.legacy {
				token = f.worker(t, "legacy")
			}
			report := policy.Observation{Descriptor: candidate, Source: source}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), test.status)
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			if test.applied {
				historyCount++
				if setup.Policy == nil || setup.Policy.Policy.ID != candidate.ID || len(setup.ObservedPolicies) != 0 {
					t.Fatalf("default-branch apply left pending approval: %+v", setup)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), http.StatusNoContent)
			} else if len(setup.ObservedPolicies) != 1 || setup.Policy == nil || setup.Policy.Policy.ID != original.ID || !reflect.DeepEqual(setup.ObservedPolicies[0].Source, source) {
				t.Fatalf("unapplied revision changed approval or lost report: %+v", setup)
			}
			if test.replay {
				previousSource := *source
				previousSource.Commit = original.Workflow.Revision
				previousReport := policy.Observation{Descriptor: original, Source: &previousSource}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, previousReport), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), http.StatusNoContent)
				historyCount += 2
			}
			var approval policy.Approval
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &approval)
			if len(approval.History) != historyCount {
				t.Fatalf("history count = %d, want %d", len(approval.History), historyCount)
			}
			if test.applied {
				entry := approval.History[0]
				previous := ""
				if test.initial {
					previous = original.SourceDigest
				}
				if entry.Repository != "acme/orders" || entry.Commit != candidate.Workflow.Revision || entry.PreviousDefinitionDigest != previous || entry.DefinitionDigest != candidate.SourceDigest || entry.RunnerID != runner.binding.RunnerID || entry.AppliedBy != runner.binding.RunnerID || entry.AppliedAt == "" {
					t.Fatalf("incomplete apply history: %+v", entry)
				}
			}
			if test.applied && test.initial {
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy?limit=1", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var firstPage policy.Approval
				decodeHubResponse(t, response, &firstPage)
				if len(firstPage.History) != 1 || !reflect.DeepEqual(firstPage.History[0], approval.History[0]) || firstPage.HistoryNext == "" {
					t.Fatalf("first history page = %+v", firstPage)
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy?limit=1&after="+firstPage.HistoryNext, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var nextPage policy.Approval
				decodeHubResponse(t, response, &nextPage)
				if len(nextPage.History) != 1 || !reflect.DeepEqual(nextPage.History[0], approval.History[1]) {
					t.Fatalf("next history page = %+v", nextPage)
				}
			}
			project, err := readNativeProject(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			wantStates := nativeFixtureStates()
			if test.applied {
				wantStates = candidate.Workflow.States
			}
			if err != nil || !reflect.DeepEqual(project.States, wantStates) {
				t.Fatalf("workflow states = %+v, %v; want %+v", project.States, err, wantStates)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var retained = item
			decodeHubResponse(t, response, &retained)
			if !reflect.DeepEqual(retained, item) {
				t.Fatalf("apply remapped or edited work item: %+v -> %+v", item, retained)
			}
		})
	}
}
