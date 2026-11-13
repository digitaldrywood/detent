package hubserver

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalHubStoresRunDetailsInPostgres(t *testing.T) {
	t.Parallel()
	service := newLocalHTTPService(t, func(cfg *Config) { cfg.Content, cfg.RunDetails = nil, nil })
	if service.config.Hosted != nil || service.config.Content != nil {
		t.Fatal("fixture is not a local Hub without a content owner")
	}
	if _, ok := service.runDetailStore().(*hubmaintenance.Details); !ok {
		t.Fatalf("local Hub run detail store = %T", service.runDetailStore())
	}
	value, ok := projectRecordDatabases.Load(service.database.postgres)
	if !ok {
		t.Fatal("local Hub has no isolated PostgreSQL database")
	}
	admin := value.(*sql.DB)
	f := newNativeFixture(t, service, "", "Example project")
	approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "local run detail")
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	lease := nativeAttemptFixtureLease(t, runner, issue, "local-run-detail")
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
	start := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, runner.redemption.Credential, start), http.StatusOK)
	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "local-run-detail-checkpoint", 2
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, runner.redemption.Credential, checkpoint), http.StatusOK)

	response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/attempts/"+start.Data.AttemptID, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var attempt tracker.NativeAttempt
	decodeHubResponse(t, response, &attempt)
	if attempt.AttemptID != start.Data.AttemptID || attempt.RunID != start.Data.RunID || attempt.Sequence != 2 || attempt.Identity == nil || attempt.Identity.Model != "test-model" {
		t.Fatalf("run detail not read back: %+v", attempt)
	}
	if attempt.Checkpoint == nil || attempt.Checkpoint.Resume != "resume_session" || attempt.Checkpoint.WorktreeState != "dirty" {
		t.Fatalf("checkpoint detail not read back: %+v", attempt.Checkpoint)
	}
	var payloads, checkpoints int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE d.object_key=a.payload_object_key),count(*) FILTER (WHERE d.object_key=a.checkpoint_object_key)
FROM native_attempts a JOIN hub_detail_objects d ON d.organization_id=a.organization_id
WHERE a.organization_id=$1 AND a.id=$2`, f.project.OrganizationID, start.Data.AttemptID).Scan(&payloads, &checkpoints); err != nil {
		t.Fatal(err)
	}
	if payloads != 1 || checkpoints != 1 {
		t.Fatalf("run detail objects in PostgreSQL: payload=%d checkpoint=%d", payloads, checkpoints)
	}
}
