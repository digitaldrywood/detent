package hubserver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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
			if !barrier.Red || barrier.Running || barrier.Repair != "" || len(barrier.Changes) != 2 || barrier.Result.Output != red.Output {
				t.Fatalf("red barrier=%+v", barrier)
			}
			var filed int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE title LIKE '%landing barrier%'").Scan(&filed); err != nil || filed != 0 {
				t.Fatalf("red barrier filed %d issues, err=%v", filed, err)
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
			if repeated := mutate("red-again", "finish", next.ID, red, ""); !repeated.Red || repeated.Repair != "" {
				t.Fatalf("repeated red=%+v", repeated)
			}
			if idle := mutate("idle-red", "start", "", nil, checked); idle.Running {
				t.Fatal("barrier reran an unchanged head")
			}
			land("repair", f.create(t, "repair"))
			next = mutate("repair-start", "start", "", nil, landed)
			green := *red
			green.ExitCode, green.Output = 0, "passed"
			repairAt := f.service.config.now()
			repairCommand := gate.CommandResult{Command: "make verify", HeadSHA: checked, TreeSHA: red.TreeSHA, ExitCode: 0, StartedAt: repairAt, FinishedAt: repairAt.Add(2 * time.Second)}
			green.Pipeline = &gate.PipelineEvidence{Timings: []gate.PipelineTiming{repairCommand.PipelineTiming("repair")}}
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
			window := operatortool.AnalyticsWindow{From: repairAt.Add(-time.Hour), To: repairAt.Add(time.Hour)}
			timingReport := nativeAnalyticsProject{Window: window}
			if err := readPipelineBarriers(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, &timingReport); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, stage := range summarizePipelineTimings(timingReport.pipelineTimings, window).Stages {
				if stage.Stage == "repair" {
					found = stage.Executed == 1 && stage.Duration.Seconds == 2
				}
			}
			if !found {
				t.Fatal("normal barrier completion lost or duplicated repair timing")
			}
			land("after-green", f.create(t, "after-green"))
			next = mutate("new-start", "start", "", nil, landed)
			if newRed := mutate("new-red", "finish", next.ID, red, ""); !newRed.Red || newRed.GreenHead != outOfBand || newRed.Repair != "" {
				t.Fatalf("new red=%+v", newRed)
			}

		})
	}
}

func TestBarrierPrefersTheLargestOnlineRunner(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	large := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	large.redemption.Capacity = 8
	large.enroll(t)
	small := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	small.redemption.Hostname, small.redemption.Capacity = "laptop", 4
	small.enroll(t)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	now := f.service.config.now()
	for _, test := range []struct {
		name    string
		problem bool
		offline bool
		want    string
	}{
		{name: "largest online runner owns the barrier", want: large.binding.RunnerID},
		{name: "a reported problem does not hand the barrier to a smaller runner", problem: true, want: large.binding.RunnerID},
		{name: "next runner takes over when the largest is offline", offline: true, want: small.binding.RunnerID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.problem {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET problems_json=? WHERE id=?", `[{"code":"settings_invalid","message":"transient","reported_at":"`+formatHubTime(now)+`"}]`, large.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			if test.offline {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(now.Add(-time.Hour)), large.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := preferredBarrierRunner(t.Context(), f.service.database.db, scope, now)
			if err != nil || got != test.want {
				t.Fatalf("preferred=%q error=%v, want %q", got, err, test.want)
			}
		})
	}
}

func TestBarrierStartIsRefusedToSmallerRunner(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "barrier-pin")
	descriptor := hubTestPolicy()
	descriptor.Gates.LandingMode = gate.LandingRollingBarrier
	descriptor.Gates.LandingCommandDigest = policy.Digest([]byte("make verify"))
	descriptor = descriptor.WithID()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	repository := "https://github.com/example/repo"
	item := f.create(t, "landed")
	base := f.base + "/work-items/" + string(item.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, base, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "landed"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	input := changeTestInput()
	input.PolicyID, input.Repository = descriptor.ID, repository
	response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version"}, ChangeVersionInput: input})
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/"+change.ID+"/versions/"+version.ID+"/landing", f.token, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "landing"}, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}), http.StatusOK)
	large := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	large.redemption.Capacity = 8
	large.enroll(t)
	small := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	small.redemption.Hostname, small.redemption.Capacity = "laptop", 4
	small.enroll(t)
	for _, test := range []struct {
		name    string
		runner  runnerFixture
		started bool
		recover bool
	}{
		{name: "smaller runner is refused", runner: small},
		{name: "largest runner starts", runner: large, started: true},
		{name: "smaller runner cannot recover the largest runner's barrier", runner: small, recover: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := "start-" + strings.ReplaceAll(test.name, " ", "-")
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/landing-barrier", test.runner.redemption.Credential, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, Repository: repository, PolicyID: descriptor.ID, Action: "start", Head: strings.Repeat("e", 40), Recover: test.recover})
			requireNativeStatus(t, response, http.StatusOK)
			var barrier tracker.LandingBarrier
			decodeHubResponse(t, response, &barrier)
			if started := barrier.Running && barrier.ID == key; started != test.started {
				t.Fatalf("started=%v barrier=%+v", started, barrier)
			}
		})
	}
}

func TestLargestRunnerTakesOverSmallerRunnersBarrier(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "barrier-takeover")
	descriptor := hubTestPolicy()
	descriptor.Gates.LandingMode = gate.LandingRollingBarrier
	descriptor.Gates.LandingCommandDigest = policy.Digest([]byte("make verify"))
	descriptor = descriptor.WithID()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	repository := "https://github.com/example/repo"
	item := f.create(t, "landed")
	base := f.base + "/work-items/" + string(item.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, base, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "landed"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	input := changeTestInput()
	input.PolicyID, input.Repository = descriptor.ID, repository
	response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version"}, ChangeVersionInput: input})
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/"+change.ID+"/versions/"+version.ID+"/landing", f.token, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "landing"}, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}), http.StatusOK)
	small := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	small.redemption.Hostname, small.redemption.Capacity = "laptop", 4
	small.enroll(t)
	start := func(r runnerFixture, key string) tracker.LandingBarrier {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/landing-barrier", r.redemption.Credential, tracker.LandingBarrierRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, Repository: repository, PolicyID: descriptor.ID, Action: "start", Head: strings.Repeat("e", 40)})
		requireNativeStatus(t, response, http.StatusOK)
		var barrier tracker.LandingBarrier
		decodeHubResponse(t, response, &barrier)
		return barrier
	}
	if held := start(small, "small-start"); !held.Running || held.Owner != small.binding.RunnerID {
		t.Fatalf("only runner did not start the barrier: %+v", held)
	}
	large := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	large.redemption.Capacity = 8
	large.enroll(t)
	observe := func(r runnerFixture) tracker.LandingBarrier {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/landing-barrier?repository="+url.QueryEscape(repository), r.redemption.Credential, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var barrier tracker.LandingBarrier
		decodeHubResponse(t, response, &barrier)
		return barrier
	}
	if seen := observe(large); seen.Running {
		t.Fatalf("preferred runner sees the smaller runner's barrier as running, so it never asks to take over: %+v", seen)
	}
	if seen := observe(small); !seen.Running {
		t.Fatalf("owner no longer sees its running barrier: %+v", seen)
	}
	if taken := start(large, "large-start"); !taken.Running || taken.ID != "large-start" || taken.Owner != large.binding.RunnerID {
		t.Fatalf("largest runner did not take over: %+v", taken)
	}
}
