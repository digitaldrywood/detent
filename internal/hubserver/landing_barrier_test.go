package hubserver

import (
	"net/http"
	"strconv"
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
			if !barrier.Red || barrier.Running || len(barrier.Changes) != 2 {
				t.Fatalf("red barrier=%+v", barrier)
			}
			credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
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
			if !repeated.Red || len(repeated.History) != 2 {
				t.Fatalf("red history=%+v", repeated)
			}
			next = mutate("idle-red", "start", "", nil, checked)
			if !next.Running {
				t.Fatal("red barrier cannot retry repair on unchanged head")
			}
			mutate("retry-cancel", "cancel", next.ID, nil, "")
			passing := f.create(t, "passing merging change")
			change, version := publish("passing", passing)
			versionPath := f.base + "/work-items/" + string(passing.WorkItemID) + "/changes/" + change.ID + "/versions/" + version.ID
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "passing-review"}, Decision: "approved"}), http.StatusOK)
			check := changeTestResult(version)
			check.IdempotencyKey = "passing-check"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/checks", f.token, check), http.StatusOK)
			issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(passing.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			issue.State = "Merging"
			if _, err := persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{}, f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			var id tracker.WorkItemID
			if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", passing.WorkItemID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			ready, evaluated, err := nativeLandingCandidateReady(t.Context(), tx, &scope, id, "machine", f.service.config.now(), false)
			if err != nil || !ready || !evaluated {
				t.Fatalf("red landing ready=%v evaluated=%v error=%v", ready, evaluated, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/landing", f.token, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "passing-land"}, MergeSHA: landed, BaseRef: "main", Method: "squash"}), http.StatusOK)
			next = mutate("repair-start", "start", "", nil, landed)
			green := *red
			green.ExitCode, green.Output = 0, "passed"
			recorded := mutate("passing-record", "record", next.ID, &green, "")
			if !recorded.Red || !recorded.Running {
				t.Fatal("intermediate passing check cleared the red barrier before repair landing")
			}
			repairEvidence := tracker.LandingBarrierRepair{HeadSHA: checked, ThreadID: "repair-thread", Output: "staged repair"}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/landing-barrier", f.token, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: "repair-evidence"}, Action: "record", Repository: repository, ID: next.ID, Repair: &repairEvidence})
			requireNativeStatus(t, response, http.StatusOK)
			finished := mutate("green", "finish", next.ID, &green, "")
			if finished.Red || finished.Running {
				t.Fatalf("green=%+v", finished)
			}
			covered, err = withLandingBarrier(t.Context(), f.service.database.db, tracker.NativeLandingReceipt{Landed: true, VersionID: first.ID})
			if err != nil || covered.Barrier == nil || covered.Barrier.ExitCode != 0 || len(covered.BarrierHistory) != 3 || covered.BarrierHistory[1].Repair == nil || covered.BarrierHistory[1].Repair.ThreadID != repairEvidence.ThreadID {
				t.Fatalf("landing receipt lost repair evidence: %+v error=%v", covered, err)
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
			if !newRed.Red || newRed.GreenHead != outOfBand || newRed.Result.Output != red.Output {
				t.Fatalf("new failure lost evidence: %+v", newRed)
			}
			var repairs int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE title LIKE 'Repair rolling landing barrier%'").Scan(&repairs); err != nil || repairs != 0 {
				t.Fatalf("barrier filed %d issues: %v", repairs, err)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/landing-barrier?repository="+repository, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var read tracker.LandingBarrier
			decodeHubResponse(t, response, &read)
			if len(read.History) != len(newRed.History) || read.Result.HeadSHA != checked || read.Result.Command != red.Command || read.Result.ExitCode != 7 {
				t.Fatalf("read evidence=%+v", read)
			}
			var repairRecorded bool
			for _, event := range read.History {
				if event.Repair != nil && event.Repair.ThreadID == repairEvidence.ThreadID && event.Repair.Output == repairEvidence.Output {
					repairRecorded = true
				}
			}
			if !repairRecorded {
				t.Fatal("barrier API lost the repair session evidence")
			}
			next = mutate("history-start", "start", "", nil, checked)
			for i := range 22 {
				evidence := repairEvidence
				evidence.ThreadID = "history-" + strconv.Itoa(i)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/landing-barrier", f.token, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: evidence.ThreadID}, Action: "record", Repository: repository, ID: next.ID, Repair: &evidence})
				requireNativeStatus(t, response, http.StatusOK)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/landing-barrier?repository="+repository, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &read)
			if len(read.History) != 20 || read.HistoryCursor == 0 || read.History[0].Repair.ThreadID != "history-2" || read.History[19].Repair.ThreadID != "history-21" {
				t.Fatalf("unbounded or unordered history=%+v", read)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/landing-barrier?repository="+repository+"&history_before="+strconv.FormatInt(read.HistoryCursor, 10), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			read = tracker.LandingBarrier{}
			decodeHubResponse(t, response, &read)
			if read.HistoryCursor != 0 || len(read.History) < 3 || read.History[0].Result == nil || read.History[0].Result.Output != red.Output || read.History[len(read.History)-1].Repair.ThreadID != "history-1" {
				t.Fatalf("older barrier evidence lost=%+v", read)
			}

		})
	}
}
