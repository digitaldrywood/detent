package hubserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSourceTransfer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                                 string
		dirty, running, expired, released, denied, stale, cancel, concurrent bool
		uncertain, prestart, cleanEffect, held, staleVersion                 bool
		status                                                               int
	}{
		{name: "transfer retains a nondispatchable lane and human label", held: true, released: true, status: http.StatusOK},
		{name: "transfer and duplicate request", released: true, status: http.StatusOK},
		{name: "cancel explicit routing", released: true, cancel: true, status: http.StatusOK},
		{name: "concurrent transfer revisions", released: true, concurrent: true, status: http.StatusOK},
		{name: "active execution", running: true, status: http.StatusUnprocessableEntity},
		{name: "partition and lease expiry do not prove quiescence", running: true, expired: true, status: http.StatusUnprocessableEntity},
		{name: "terminal execution still owns lease", status: http.StatusUnprocessableEntity},
		{name: "legacy dirty changes no longer pin the source", dirty: true, released: true, status: http.StatusOK},
		{name: "write grant does not grant runner management", denied: true, released: true, status: http.StatusForbidden},
		{name: "clean startup with uncertain publication", cleanEffect: true, uncertain: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "uncertain publication outcome", uncertain: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "claim before startup is not quiesced", prestart: true, released: true, status: http.StatusOK},
		{name: "stale source version", staleVersion: true, released: true, status: http.StatusUnprocessableEntity},
		{name: "stale revision", stale: true, released: true, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}))
			var diagnostics bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewTextHandler(&diagnostics, nil))
			a := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			b := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			a.redemption.DisplayName, b.redemption.DisplayName = "Source Air", "Destination Mini"
			a.enroll(t)
			b.enroll(t)
			claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: f.issue.WorkItemID, MachineID: a.binding.MachineID, SessionID: "source", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", a.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			event := nativeStartedEvent(lease)
			itemPath := f.base + "/work-items/" + string(f.issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.WorktreeState = "unpushed"
			event.Data.Handoff.HeadSHA = changeTestInput().HeadSHA
			if test.cleanEffect {
				event.Data.Handoff.WorktreeState, event.Data.Handoff.Resume = "clean", "fresh_checkout"
			}
			if test.uncertain {
				event.Data.Handoff.ExternalEffect = "pr_create"
				event.Data.Handoff.EffectState = "ambiguous"
				event.Data.Handoff.EffectID = "effect_" + strings.Repeat("e", 32)
			}
			if test.dirty {
				event.Data.Handoff.WorktreeState = "dirty"
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusOK)
			input := changeTestInput()
			input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
			bundle := []byte("retained source fixture")
			input.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source"}, ChangeVersionInput: input, SourceBundle: bundle})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			if !test.running {
				finish := event
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome, finish.Data.Handoff = "run.finished", "finished", 3, "succeeded", nil
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, finish), http.StatusOK)
			}
			if test.released {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", a.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			}
			if test.expired {
				now = now.Add(91 * time.Second)
			}
			if test.held {
				labels := []string{"human-owned"}
				edited := performHubAPIRequest(t, f.service, http.MethodPatch, itemPath, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "human-hold"}, ExpectedRevision: f.issue.Revision, Labels: &labels})
				requireNativeStatus(t, edited, http.StatusOK)
				decodeHubResponse(t, edited, &f.issue)
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET dispatchable=0 WHERE project_id=? AND detent_state=?", f.project.ID, f.issue.State); err != nil {
					t.Fatal(err)
				}
			}

			recoveryRead := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/source-recovery", f.token, nil)
			requireNativeStatus(t, recoveryRead, http.StatusOK)
			var observed sourceRecoveryView
			decodeHubResponse(t, recoveryRead, &observed)
			if observed.SourceRunnerName != "Source Air" {
				t.Fatalf("scoped source runner name missing: %q", observed.SourceRunnerName)
			}
			explanationRead := performHubAPIRequest(t, f.service, http.MethodGet, itemPath+"/explanation", f.token, nil)
			requireNativeStatus(t, explanationRead, http.StatusOK)
			var explanation struct {
				Runtime *tracker.NativeRuntimeEvidence `json:"native_runtime"`
			}
			decodeHubResponse(t, explanationRead, &explanation)
			if explanation.Runtime == nil || explanation.Runtime.SourceRecovery == nil || explanation.Runtime.SourceRecovery.AttemptID != observed.AttemptID || explanation.Runtime.SourceRecovery.SourceRunnerName != observed.SourceRunnerName || explanation.Runtime.SourceRecovery.Quiesced != observed.Quiesced || explanation.Runtime.SourceRecovery.Reason != observed.Reason {
				t.Fatal("existing explanation lost source ownership or waiting reason")
			}

			request := sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "transfer"}, ExpectedRevision: f.issue.Revision, VersionID: version.ID, DestinationRunnerID: b.binding.RunnerID}
			if test.staleVersion {
				request.VersionID = "version_stale"
			}
			if test.stale {
				request.ExpectedRevision++
			}
			token := testHubAdminToken
			if test.denied {
				token = f.token
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request)
			if response.Code >= 500 {
				t.Fatalf("transfer failed: %s", diagnostics.String())
			}
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				var destination string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT recovery_runner_id FROM issues WHERE native_id=?", f.issue.WorkItemID).Scan(&destination); err != nil || destination != "" {
					t.Fatalf("refusal changed owner: %q %v", destination, err)
				}
				return
			}
			var view sourceRecoveryView
			decodeHubResponse(t, response, &view)
			if view.SourceRunnerID != a.binding.RunnerID || view.DestinationRunnerID != b.binding.RunnerID || !view.Available || !view.Quiesced || view.HeadSHA != input.HeadSHA || view.BaseSHA != input.BaseSHA {
				t.Fatalf("transfer lost source identity: %#v", view)
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("duplicate transfer changed receipt")
			}
			var audited int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND json_extract(data_json,'$.operation')='source_recovery'", f.issue.WorkItemID).Scan(&audited); err != nil || audited != 1 {
				t.Fatalf("duplicate audit events: %d %v", audited, err)
			}
			current, _, err := readNativeIssue(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}, string(f.issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			if current.State != f.issue.State || current.Archived != f.issue.Archived || !reflect.DeepEqual(current.Labels, f.issue.Labels) {
				t.Fatal("transfer changed workflow state or archive hold")
			}
			if test.name == "transfer and duplicate request" {
				body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}, "name": operatortool.TransferItem, "arguments": map[string]any{"project_id": f.project.ID, "identifier": f.issue.WorkItemID, "request_id": "mcp-transfer", "expected_revision": strconv.FormatInt(int64(view.Revision), 10), "version_id": version.ID, "destination_runner_id": b.binding.RunnerID}}}
				path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
				for _, credential := range []string{f.token, testHubAdminToken} {
					mcp := performHubWorkCall(t, f.service, path, credential, operatortool.TransferItem, body)
					requireNativeStatus(t, mcp, http.StatusOK)
					var envelope struct {
						Result struct {
							IsError bool `json:"isError"`
							Content struct {
								Data sourceRecoveryView `json:"data"`
							} `json:"structuredContent"`
						} `json:"result"`
					}
					if err := json.Unmarshal(mcp.Body.Bytes(), &envelope); err != nil {
						t.Fatal(err)
					}
					if credential == f.token {
						if !envelope.Result.IsError {
							t.Fatal("MCP bypassed runner administration")
						}
						continue
					}
					if envelope.Result.IsError || envelope.Result.Content.Data.DestinationRunnerID != b.binding.RunnerID {
						t.Fatalf("MCP transfer failed: %s", mcp.Body.String())
					}
					view = envelope.Result.Content.Data
					duplicate := performHubWorkCall(t, f.service, path, credential, operatortool.TransferItem, body)
					original := envelope.Result.Content.Data
					if err := json.Unmarshal(duplicate.Body.Bytes(), &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.Result.IsError || !reflect.DeepEqual(envelope.Result.Content.Data, original) {
						t.Fatalf("MCP transfer replay changed receipt: %s", duplicate.Body.String())
					}
				}
			}
			if test.held {
				claim.SessionID = "human-held"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", a.redemption.Credential, claim), http.StatusConflict)
				claim.MachineID = b.binding.MachineID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", b.redemption.Credential, claim), http.StatusConflict)
				return
			}

			if test.prestart {
				claim.MachineID, claim.SessionID = b.binding.MachineID, "destination-before-start"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", b.redemption.Credential, claim), http.StatusOK)
				request.IdempotencyKey, request.ExpectedRevision, request.DestinationRunnerID = "cancel-active", view.Revision, ""
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request), http.StatusUnprocessableEntity)
				return
			}
			if test.cancel {
				request.IdempotencyKey, request.ExpectedRevision, request.DestinationRunnerID = "cancel", view.Revision, ""
				response = performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, request)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &view)
				if view.DestinationRunnerID != "" {
					t.Fatal("cancellation retained destination")
				}
				return
			}
			if test.concurrent {
				var wg sync.WaitGroup
				statuses := make(chan int, 2)
				for i, destination := range []string{a.binding.RunnerID, b.binding.RunnerID} {
					wg.Go(func() {
						next := request
						next.IdempotencyKey, next.ExpectedRevision, next.DestinationRunnerID = "concurrent-"+destination, view.Revision, destination
						if i == 0 {
							next.IdempotencyKey += "-a"
						}
						statuses <- performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/source-recovery", token, next).Code
					})
				}
				wg.Wait()
				close(statuses)
				successes, conflicts := 0, 0
				for status := range statuses {
					switch status {
					case http.StatusOK:
						successes++
					case http.StatusConflict:
						conflicts++
					default:
						t.Fatalf("concurrent status=%d", status)
					}
				}
				if successes != 1 || conflicts != 1 {
					t.Fatalf("concurrent transfers succeeded=%d conflicted=%d", successes, conflicts)
				}
				return
			}
			claim.MachineID, claim.SessionID = b.binding.MachineID, "destination"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", b.redemption.Credential, claim), http.StatusOK)
			event.IdempotencyKey, event.Data.Sequence = "late-owner", 4
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", a.redemption.Credential, event), http.StatusConflict)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", a.redemption.Credential, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "late-publication", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedVersionID: version.ID, ChangeVersionInput: input, SourceBundle: bundle}), http.StatusConflict)
		})
	}
}
