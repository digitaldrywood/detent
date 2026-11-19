package runnerruntime_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFencedRunEventRecordsWorkerInventory(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewChange(t, hubfixture.Open(t, hubfixture.Config{}))
	runner := hubfixture.PrepareRunner(t, f.Native, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.Enroll(t)
	hubfixture.PublishCapacity(t, f.Native, runner, hubfixture.CapacityReport(f.Hub.Now()))
	token := runner.Redemption.Credential
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", token, hubfixture.ProviderClaim(runner, f.Issue, "session"))
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, response, &lease)
	start := hubfixture.StartedEvent(lease)
	worker := tracker.NativeAttemptWorker{PID: 101, PGID: 101, StartedAt: f.Hub.Now().UTC().Truncate(time.Microsecond), StartKey: "kernel-birth"}
	start.Worker = &worker
	path := f.Base + "/work-items/" + string(f.Issue.WorkItemID) + "/events"
	send := func(event tracker.NativeRunEvent, want int) {
		t.Helper()
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, token, event), want)
	}
	oversize := worker
	oversize.StartKey = strings.Repeat("é", tracker.MaxAttemptWorkerStartKeyBytes/2+1)
	badStart := start
	badStart.Worker = &oversize
	send(badStart, http.StatusUnprocessableEntity)
	imprecise := worker
	imprecise.StartedAt = worker.StartedAt.Add(time.Nanosecond)
	badStart.Worker = &imprecise
	send(badStart, http.StatusUnprocessableEntity)
	sendSubnanosecond := func(event tracker.NativeRunEvent) {
		t.Helper()
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		formatted := worker.StartedAt.Format(time.RFC3339Nano)
		overprecise := worker.StartedAt.Format("2006-01-02T15:04:05.000000") + "0001Z"
		body = bytes.Replace(body, []byte(`"started_at":"`+formatted+`"`), []byte(`"started_at":"`+overprecise+`"`), 1)
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, token, json.RawMessage(body)), http.StatusUnprocessableEntity)
	}
	sendSubnanosecond(start)
	send(start, http.StatusOK)
	send(start, http.StatusOK)

	other := worker
	other.StartKey = "different-kernel-birth"
	replay := start
	replay.Worker = &other
	send(replay, http.StatusConflict)
	replay.IdempotencyKey = "conflicting-start-replay"
	send(replay, http.StatusConflict)
	heartbeat := start
	heartbeat.IdempotencyKey, heartbeat.Type, heartbeat.Data.Sequence, heartbeat.Worker = "observe", "run.observed", 2, nil
	heartbeat.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: f.Hub.Now()}
	stale := heartbeat
	stale.IdempotencyKey = "observe-stale"
	stale.Data.FencingToken++
	stale.Worker = &other
	send(stale, http.StatusConflict)
	send(heartbeat, http.StatusOK)
	replacement := heartbeat
	replacement.IdempotencyKey, replacement.Data.Sequence, replacement.Worker = "observe-replacement", 3, &other
	send(replacement, http.StatusConflict)
	replacement.IdempotencyKey, replacement.Worker = "observed-owner-confirmed", &worker
	send(replacement, http.StatusOK)

	read := func() runnerauth.AttemptWorkerPage {
		t.Helper()
		response := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(runner.Binding.MachineID)+"/heartbeat?attempt_worker_after=", token, struct{}{})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var inventory runnerauth.AttemptWorkerInventory
		hubfixture.Decode(t, response, &inventory)
		if inventory.Page == nil || inventory.Page.ProjectID != f.Project.ID || inventory.Page.Next != "" {
			t.Fatalf("heartbeat worker page = %+v", inventory.Page)
		}
		return *inventory.Page
	}
	page := read()
	if len(page.Workers) != 1 {
		t.Fatalf("heartbeat worker inventory = %+v", page)
	}
	owner := page.Workers[0]
	if owner.AttemptID != start.Data.AttemptID || owner.LeaseID != lease.ID || owner.FencingToken != lease.FencingToken ||
		owner.LeaseState != runnerauth.AttemptLeaseLive || owner.Worker.PID != worker.PID || owner.Worker.PGID != worker.PGID ||
		owner.Worker.StartKey != worker.StartKey || !owner.Worker.StartedAt.Equal(worker.StartedAt) {
		t.Fatalf("heartbeat lost the fenced owner: %+v", owner)
	}

	cleanup := tracker.NativeRunEvent{Type: "run.finished", SchemaVersion: 1, WorkerCleanup: &worker,
		Data: tracker.NativeRunData{AttemptID: owner.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}}
	sendSubnanosecond(cleanup)
	wrongFence := cleanup
	wrongFence.Data.FencingToken++
	send(wrongFence, http.StatusConflict)
	wrongWorker := cleanup
	wrongWorker.WorkerCleanup = &other
	send(wrongWorker, http.StatusConflict)
	wrongWorker.WorkerCleanup = &imprecise
	send(wrongWorker, http.StatusUnprocessableEntity)
	later := worker
	later.StartedAt = worker.StartedAt.Add(time.Microsecond)
	wrongWorker.WorkerCleanup = &later
	send(wrongWorker, http.StatusConflict)
	if page := read(); len(page.Workers) != 1 || page.Workers[0] != owner {
		t.Fatalf("stale cleanup changed inventory: %+v", page)
	}
	send(cleanup, http.StatusOK)
	send(cleanup, http.StatusOK)
	if page := read(); len(page.Workers) != 0 {
		t.Fatalf("retired owner remains in inventory: %+v", page)
	}
	replacement.IdempotencyKey, replacement.Data.Sequence, replacement.Worker = "replace-retired-owner", 4, &other
	send(replacement, http.StatusOK)
	send(cleanup, http.StatusConflict)
	if page := read(); len(page.Workers) != 1 || page.Workers[0].Worker.StartKey != other.StartKey {
		t.Fatalf("replacement missing after stale cleanup: %+v", page)
	}
}
