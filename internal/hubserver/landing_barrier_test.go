package hubserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRollingLandingBarrier(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{gate.LandingPerChange, gate.LandingRollingBarrier} {
		t.Run(mode, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "barrier")
			descriptor := hubTestPolicy()
			descriptor.Gates.LandingMode = mode
			descriptor.Gates.LandingCommandDigest = policy.Digest([]byte("make verify"))
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			repository := "https://github.com/example/repo"
			publish := func(key string, item tracker.NativeIssue) (tracker.ChangeRequest, tracker.ChangeVersion) {
				t.Helper()
				base := f.base + "/work-items/" + string(item.WorkItemID) + "/changes"
				response := performHubAPIRequest(t, f.service, http.MethodPost, base, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: key + ":change"}, Title: key})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				input := changeTestInput()
				input.PolicyID, input.Repository = descriptor.ID, repository
				response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key + ":version"}, ChangeVersionInput: input})
				requireNativeStatus(t, response, http.StatusOK)
				var version tracker.ChangeVersion
				decodeHubResponse(t, response, &version)
				return change, version
			}
			land := func(key string, item tracker.NativeIssue) tracker.ChangeVersion {
				t.Helper()
				change, version := publish(key, item)
				path := f.base + "/work-items/" + string(item.WorkItemID) + "/changes/" + change.ID + "/versions/" + version.ID + "/landing"
				request := tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key + ":landing"}, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}
				for range 2 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request), http.StatusOK)
				}
				return version
			}
			mutate := func(key, action, id string, result *gate.CommandResult, head string) tracker.LandingBarrier {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/landing-barrier", f.token, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, Repository: repository, PolicyID: descriptor.ID, Action: action, ID: id, Result: result, Head: head})
				requireNativeStatus(t, response, http.StatusOK)
				var barrier tracker.LandingBarrier
				decodeHubResponse(t, response, &barrier)
				return barrier
			}
			firstItem := f.create(t, "first")
			first := land("first", firstItem)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM landing_barrier_receipts WHERE version_id=?", first.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if mode == gate.LandingPerChange {
				if count != 0 {
					t.Fatalf("per-landing created %d barrier receipts", count)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/landing-barrier", f.token, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Repository: repository, PolicyID: descriptor.ID, Action: "start"})
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				return
			}
			if count != 1 {
				t.Fatalf("landing created %d receipts", count)
			}
			checked, landed, outOfBand := strings.Repeat("e", 40), strings.Repeat("1", 40), strings.Repeat("2", 40)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/landing-barrier", f.token, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: "headless"}, Repository: repository, PolicyID: descriptor.ID, Action: "start"}), http.StatusUnprocessableEntity)
			barrier := mutate("start", "start", "", nil, landed)
			if !barrier.Running || len(barrier.Changes) != 1 || barrier.Changes[0].VersionID != first.ID {
				t.Fatalf("barrier=%+v", barrier)
			}
			if overlap := mutate("overlap", "start", "", nil, landed); overlap.ID != barrier.ID {
				t.Fatalf("overlapping barrier=%+v", overlap)
			}
			secondItem := f.create(t, "second")
			second := land("second", secondItem)
			red := &gate.CommandResult{Command: "make verify", HeadSHA: checked, TreeSHA: strings.Repeat("f", 40), ExitCode: 7, Output: "integration failure sentinel"}
			barrier = mutate("red", "finish", barrier.ID, red, "")
			if !barrier.Red || barrier.Running || barrier.Repair == "" || len(barrier.Changes) != 2 {
				t.Fatalf("red barrier=%+v", barrier)
			}
			credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
			repair, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(barrier.Repair))
			if err != nil || repair.State != "Todo" || repair.Priority == nil || *repair.Priority != 1 || !strings.Contains(repair.Body, first.ID) || !strings.Contains(repair.Body, second.ID) || !strings.Contains(repair.Body, red.Output) {
				t.Fatalf("repair=%+v error=%v", repair, err)
			}
			covered, err := withLandingBarrier(t.Context(), f.service.database.db, tracker.NativeLandingReceipt{Landed: true, VersionID: first.ID})
			if err != nil || covered.Barrier == nil || covered.Barrier.HeadSHA != red.HeadSHA || covered.Barrier.ExitCode != 7 {
				t.Fatalf("coverage=%+v error=%v", covered, err)
			}
			uncovered, err := withLandingBarrier(t.Context(), f.service.database.db, tracker.NativeLandingReceipt{Landed: true, VersionID: second.ID})
			if err != nil || uncovered.Barrier != nil {
				t.Fatalf("later landing covered early: %+v error=%v", uncovered, err)
			}
			next := mutate("next", "start", "", nil, landed)
			if !next.Running || next.ID == barrier.ID || len(next.Changes) != 2 {
				t.Fatalf("next=%+v", next)
			}
			repeated := mutate("red-again", "finish", next.ID, red, "")
			if repeated.Repair != barrier.Repair {
				t.Fatalf("duplicate repair=%+v", repeated)
			}
			var occurrences int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", barrier.Repair).Scan(&occurrences); err != nil || occurrences != 1 {
				t.Fatalf("occurrences=%d error=%v", occurrences, err)
			}
			if idle := mutate("idle-red", "start", "", nil, checked); idle.Running {
				t.Fatal("barrier reran an unchanged head")
			}
			land("repair", repair)
			next = mutate("repair-start", "start", "", nil, landed)
			green := *red
			green.ExitCode, green.Output = 0, "passed"
			finished := mutate("green", "finish", next.ID, &green, "")
			if finished.Red || finished.Running || finished.Repair != "" {
				t.Fatalf("green=%+v", finished)
			}
			if idle := mutate("idle-green", "start", "", nil, checked); idle.Running {
				t.Fatal("barrier reran an unchanged head")
			}
			merged := mutate("out-of-band", "start", "", nil, outOfBand)
			if !merged.Running || len(merged.Changes) != 0 {
				t.Fatalf("out-of-band merge did not start the barrier: %+v", merged)
			}
			mergedGreen := green
			mergedGreen.HeadSHA = outOfBand
			if finished := mutate("out-of-band-green", "finish", merged.ID, &mergedGreen, ""); finished.Red || finished.GreenHead != outOfBand {
				t.Fatalf("out-of-band green=%+v", finished)
			}
			land("after-green", f.create(t, "after-green"))
			next = mutate("new-start", "start", "", nil, landed)
			newRed := mutate("new-red", "finish", next.ID, red, "")
			if newRed.Repair == "" || newRed.Repair == repair.WorkItemID {
				t.Fatalf("new red reused terminal repair: %+v", newRed)
			}
			if report, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(newRed.Repair)); err != nil || !strings.Contains(report.Body, outOfBand+".."+checked) {
				t.Fatalf("red report omits commits since the last green head: %+v %v", report, err)
			}
			historical, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(repair.WorkItemID))
			if err != nil || !historical.Terminal {
				t.Fatalf("terminal repair changed: %+v %v", historical, err)
			}

		})
	}
}
