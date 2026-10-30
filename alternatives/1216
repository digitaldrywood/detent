package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestEvidencePayloadSizeAndReadEquivalence(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Content: objects.call}))
	version := f.publish(t, "payload-version", "")
	ctx := f.service.contentContext(t.Context())
	var inlineBytes int
	for _, size := range []int{32, 256 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: version.ID, Decision: "commented", Body: strings.Repeat("x", size), CreatedAt: f.service.config.now().UTC()}
			if err := f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
				return insertChangeEvidence(ctx, tx, f.change.ID, version.ID, "review", review.ID, review)
			}); err != nil {
				t.Fatal(err)
			}
			var stored int
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT length(record_json) FROM change_evidence WHERE source_key=?", review.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if inlineBytes != 0 && stored != inlineBytes {
				t.Errorf("inline payload grew from %d to %d bytes", inlineBytes, stored)
			}
			inlineBytes = stored
			var key string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT payload_object_key FROM change_evidence WHERE source_key=?", review.ID).Scan(&key); err != nil {
				t.Fatal(err)
			}
			var got, legacy tracker.ChangeReview
			if err := decodeStorageJSON(ctx, []byte(key), &got); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &legacy); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, legacy) {
				t.Fatal("object-backed evidence differs from legacy reads")
			}
			detail, err := readChangeDetailView(ctx, f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.issue.WorkItemID), f.change.ID, f.service.config.now(), false)
			if err != nil {
				t.Fatal(err)
			}
			if got := detail.Reviews[len(detail.Reviews)-1]; got.ID != review.ID || got.Body != review.Body {
				t.Fatal("API lost evidence body")
			}
			queries := []string{"?view=recovery", "?view=recovery&detail=full"}
			if size < 128<<10 {
				queries = append(queries, "?view=recovery&projection=bounded")
			}
			for _, query := range queries {
				t.Run(query, func(t *testing.T) {
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+query, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var recovery tracker.NativeRecovery
					decodeHubResponse(t, response, &recovery)
					if recovery.ChangeDetail == nil || len(recovery.ChangeDetail.Reviews) == 0 {
						t.Fatal("recovery lost Change evidence")
					}
					got := recovery.ChangeDetail.Reviews[len(recovery.ChangeDetail.Reviews)-1]
					if got.ID != review.ID || got.Body != review.Body {
						t.Fatal("recovery lost the full review body")
					}
				})
			}
			for _, mode := range []string{"query", "header"} {
				t.Run("summary/"+mode, func(t *testing.T) {
					path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "?view=recovery"
					if mode == "query" {
						path += "&detail=summary"
					}
					request := newHubAPIRequest(t, http.MethodGet, path, f.token, nil)
					if mode == "header" {
						request.Header.Set("X-Detent-Recovery-Detail", "summary")
					}
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					requireNativeStatus(t, response, http.StatusOK)
					var recovery tracker.NativeRecovery
					decodeHubResponse(t, response, &recovery)
					if recovery.Read == nil || !slices.Contains(recovery.Read.Omitted, "change_detail.reviews") || recovery.ChangeDetail == nil || len(recovery.ChangeDetail.Reviews) != 0 {
						t.Fatal("explicit summary recovery did not reduce Change evidence")
					}
				})
			}
		})
	}
}

func TestAttemptPayloadObjectsAndTypedReads(t *testing.T) {
	t.Parallel()
	for _, size := range []int{32, 32 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			objects := &changeObjectFixture{}
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			f := claimedAttemptDiffFixture(t, false, Config{Content: objects.call, now: func() time.Time { return now }})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			if err := f.service.stopGitHubReconciliation(); err != nil {
				t.Fatal(err)
			}
			f.service.stopHealthDetector()
			f.service.outbox.stop()
			f.start(t)
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			data := tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Handoff: nativeTestCheckpoint()}
			for range 1 + size/2048 {
				data.ArtifactIDs = append(data.ArtifactIDs, newNativeID("artifact"))
			}
			artifacts := append([]string(nil), data.ArtifactIDs...)
			post := func(kind string) {
				t.Helper()
				event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("payload-%d", data.Sequence)}, Type: kind, SchemaVersion: 1, Data: data}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
			}
			post("run.checkpointed")
			data.Sequence++
			data.Handoff, data.ArtifactIDs = nil, nil
			data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now}
			post("run.observed")
			data.Sequence++
			data.Outcome = "succeeded"
			data.CompletionBody = strings.Repeat("x", size)
			data.Steps = []tracker.NativeStepReceipt{{Step: "agent_turn", PolicyID: f.policy, ReviewState: strings.Repeat("a", 64), Source: tracker.NativeCheckpoint{HeadSHA: strings.Repeat("a", 40), WorkspaceDigest: strings.Repeat("b", 64)}, CompletedAt: now, CompletionBody: "step detail"}}
			data.ProcessUsage = &procgroup.Usage{PeakMemoryBytes: 1024, UserCPUSeconds: 2, WallSeconds: 3}
			data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: 10, Output: 5, Currency: "USD", Revision: 1, From: now.Add(-time.Minute), To: now, ReportedAt: now}}
			post("run.finished")
			var inline int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT length(data_json)+length(checkpoint_json)+length(artifact_ids_json) FROM native_attempts WHERE id=?", f.attempt).Scan(&inline); err != nil {
				t.Fatal(err)
			}
			if inline != 8 {
				t.Fatalf("inline runtime payload occupies %d bytes, want 8", inline)
			}
			f.service.database.reader.SetMaxOpenConns(1)
			defer f.service.database.reader.SetMaxOpenConns(readerConnections)
			response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+f.attempt, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var detail tracker.NativeAttempt
			decodeHubResponse(t, response, &detail)
			if detail.CompletionBody != data.CompletionBody || !reflect.DeepEqual(detail.ArtifactIDs, artifacts) || !reflect.DeepEqual(detail.Checkpoint, nativeTestCheckpoint()) {
				t.Fatal("full detail lost runtime or retained checkpoint artifacts")
			}
			for _, suffix := range []string{"/attempts", "/attempts?view=detail"} {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+suffix, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.Page[tracker.NativeAttempt]
				decodeHubResponse(t, response, &page)
				if len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], detail) {
					t.Fatalf("%s lost archived attempt detail", suffix)
				}
			}
			if len(detail.Steps) != 1 || detail.Steps[0].CompletionBody != "step detail" || detail.ProcessUsage == nil || detail.ProcessUsage.PeakMemoryBytes != 1024 || len(detail.Usage) != 1 || detail.Usage[0].Input != 10 || detail.Runtime == nil || detail.Runtime.Phase != "implementation" {
				t.Fatal("full detail lost steps, process usage, provider usage or runtime")
			}
			objects.mu.Lock()
			before := objects.reads
			objects.mu.Unlock()
			for _, authority := range []struct {
				name    string
				policy  string
				fence   tracker.FencingToken
				allowed bool
			}{
				{"current", f.policy, f.lease.FencingToken, true},
				{"other policy", "other-policy", f.lease.FencingToken, false},
				{"other fence", f.policy, f.lease.FencingToken + 1, false},
			} {
				t.Run("source authority/"+authority.name, func(t *testing.T) {
					tx, err := f.service.database.reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					request := tracker.PublishChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{RunID: f.run, AttemptID: f.attempt, PolicyID: authority.policy}}
					request.LeaseID, request.FencingToken = f.lease.ID, authority.fence
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeWorker}}
					err = requireChangeRun(t.Context(), tx, scope, tracker.ChangeRequest{WorkItemID: f.issue.WorkItemID}, request)
					if (err == nil) != authority.allowed {
						t.Fatalf("source authority allowed=%v error=%v", authority.allowed, err)
					}
				})
			}
			for _, suffix := range []string{"/attempts?view=recovery", "?view=recovery&detail=summary"} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+suffix, f.token, nil), http.StatusOK)
			}
			objects.mu.Lock()
			reads := objects.reads - before
			objects.mu.Unlock()
			if reads != 0 {
				t.Fatalf("typed recovery fetched %d objects", reads)
			}
			ctx := f.service.runDetailContext(t.Context())
			f.service.database.reader.SetMaxOpenConns(readerConnections)
			convertTestAnalyticsProjections(t, f.service)
			f.service.database.reader.SetMaxOpenConns(1)
			objects.mu.Lock()
			beforeReads, beforeWrites := objects.reads, objects.writes
			objects.mu.Unlock()
			for {
				more, err := f.service.database.drainAnalyticsDirty(ctx, now)
				if err != nil {
					t.Fatal(err)
				}
				if !more {
					break
				}
			}
			objects.mu.Lock()
			afterReads, afterWrites := objects.reads, objects.writes
			objects.mu.Unlock()
			if afterReads != beforeReads || afterWrites != beforeWrites {
				t.Fatal("analytics fetched or duplicated the source payload")
			}
			var sourceKey, derivedKey string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT a.payload_object_key,r.payload_object_key FROM native_attempts a JOIN analytics_hourly_rollups r ON r.dimension=a.id AND r.metric='attempt' WHERE a.id=?", f.attempt).Scan(&sourceKey, &derivedKey); err != nil || sourceKey == "" || sourceKey != derivedKey {
				t.Fatalf("analytics did not retain the source object reference: %v", err)
			}
			var derived string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT data_json FROM analytics_hourly_rollups WHERE metric='attempt' AND dimension=?", f.attempt).Scan(&derived); err != nil {
				t.Fatal(err)
			}
			if derived != "{}" {
				t.Fatal("analytics retained an inline attempt payload copy")
			}
			runRaw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			checkpointRaw, err := json.Marshal(nativeTestCheckpoint())
			if err != nil {
				t.Fatal(err)
			}
			artifactRaw, err := json.Marshal(artifacts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(ctx, "UPDATE native_attempts SET data_json=?,checkpoint_json=?,artifact_ids_json=?,payload_object_key='',checkpoint_object_key='',conversion_state=2 WHERE id=?", string(runRaw), string(checkpointRaw), string(artifactRaw), f.attempt); err != nil {
				t.Fatal(err)
			}
			legacy := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+f.attempt, f.token, nil)
			requireNativeStatus(t, legacy, http.StatusOK)
			for _, family := range storageConversionFamilies {
				if family.name == "attempts" || family.name == "checkpoints" {
					if err := f.service.convertObjectPayloadFamily(ctx, family, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			converted := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+f.attempt, f.token, nil)
			requireNativeStatus(t, converted, http.StatusOK)
			if legacy.Body.String() != converted.Body.String() {
				t.Fatal("backfill changed full attempt reads")
			}
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT length(data_json)+length(checkpoint_json)+length(artifact_ids_json) FROM native_attempts WHERE id=?", f.attempt).Scan(&inline); err != nil || inline != 8 {
				t.Fatalf("legacy inline bytes=%d error=%v", inline, err)
			}
			requireUsageCountersMatchRecount(t, f.service.database)
		})
	}
}

func TestVersionSourceAndDiffPayloadObjects(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{"archive", "details"} {
		t.Run(backend, func(t *testing.T) {
			objects := &changeObjectFixture{}
			config := Config{Content: objects.call}
			if backend == "details" {
				config.Content = nil
				config.RunDetails = &memoryRunDetails{}
			}
			config.DatabasePath = filepath.Join(t.TempDir(), "hub.db")
			f := newChangeFixture(t, openTestService(t, config))
			expectedVersion := ""
			for _, size := range []int{32, 32 << 10} {
				t.Run(strconv.Itoa(size), func(t *testing.T) {
					input := changeTestInput()
					bundle := changeTestSource(t, &input, []byte(strings.Repeat("x", size)))
					for index := range 1 + size/4096 {
						input.ReviewDispositions = append(input.ReviewDispositions, tracker.ReviewDisposition{ThreadID: strconv.Itoa(index), HeadSHA: input.HeadSHA, Disposition: "fixed", Evidence: strings.Repeat("x", min(size, 4096))})
					}
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: strconv.Itoa(size)}, ExpectedVersionID: expectedVersion, ChangeVersionInput: input, SourceBundle: bundle})
					requireNativeStatus(t, response, http.StatusOK)
					var version tracker.ChangeVersion
					decodeHubResponse(t, response, &version)
					expectedVersion = version.ID
					var inline int
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT length(v.record_json)+length(s.bundle) FROM change_versions v JOIN change_sources s ON s.version_id=v.id WHERE v.id=?", version.ID).Scan(&inline); err != nil || inline != 2 {
						t.Fatalf("version/source inline bytes=%d error=%v", inline, err)
					}
					ctx := f.service.contentContext(t.Context())
					restored, err := readChangeVersion(ctx, f.service.database.reader, f.change.ID, version.ID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(restored, version) {
						t.Fatal("version read differs from publication")
					}
					got, err := readChangeSourceBundle(ctx, f.service.database.reader, version)
					if err != nil {
						t.Fatal(err)
					}
					if string(got) != string(bundle) {
						t.Fatal("source object changed bundle bytes")
					}
					legacyVersion := version
					legacyVersion.ID = newNativeID("version")
					legacyVersion.Number += 1000
					raw, err := json.Marshal(legacyVersion)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(ctx, "INSERT INTO change_versions(id,change_id,number,record_json,conversion_state) VALUES(?,?,?,?,2)", legacyVersion.ID, f.change.ID, legacyVersion.Number, string(raw)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(ctx, "INSERT INTO change_sources(version_id,bundle,conversion_state) VALUES(?,?,2)", legacyVersion.ID, bundle); err != nil {
						t.Fatal(err)
					}
					before, err := readChangeVersion(ctx, f.service.database.reader, f.change.ID, legacyVersion.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, family := range storageConversionFamilies {
						if family.name == "versions" || family.name == "sources" {
							if err := f.service.convertObjectPayloadFamily(ctx, family, f.service.config.now()); err != nil {
								t.Fatal(err)
							}
						}
					}
					after, err := readChangeVersion(ctx, f.service.database.reader, f.change.ID, legacyVersion.ID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, after) {
						t.Fatal("legacy version backfill changed reads")
					}
					got, err = readChangeSourceBundle(ctx, f.service.database.reader, after)
					if err != nil || string(got) != string(bundle) {
						t.Fatalf("legacy source bytes changed: %v", err)
					}
					if err := f.service.database.reader.QueryRowContext(ctx, "SELECT length(v.record_json)+length(s.bundle) FROM change_versions v JOIN change_sources s ON s.version_id=v.id WHERE v.id=?", legacyVersion.ID).Scan(&inline); err != nil || inline != 2 {
						t.Fatalf("legacy version/source inline bytes=%d error=%v", inline, err)
					}
				})
			}
			for _, size := range []int{32, 32 << 10} {
				t.Run("diff/"+strconv.Itoa(size), func(t *testing.T) {
					d := claimedAttemptDiffFixture(t, false, config)
					d.start(t)
					patch := "@@ -1 +1 @@\n-old\n+" + strings.Repeat("x", size) + "\n"
					response := d.post(t, d.request(1, tracker.AttemptDiffFile{Path: "example.go", Status: tracker.DiffStatusModified, Patch: patch}))
					requireNativeStatus(t, response, http.StatusAccepted)
					var receipt tracker.AttemptDiffReceipt
					decodeHubResponse(t, response, &receipt)
					var inline int
					if err := d.service.database.reader.QueryRowContext(t.Context(), "SELECT coalesce(sum(length(patch)),0) FROM attempt_diff_files WHERE diff_id=?", receipt.DiffID).Scan(&inline); err != nil || inline != 0 {
						t.Fatalf("diff inline bytes=%d error=%v", inline, err)
					}
					response = d.get(t, "")
					requireNativeStatus(t, response, http.StatusOK)
					var diff tracker.AttemptDiff
					decodeHubResponse(t, response, &diff)
					if len(diff.Files) != 1 || diff.Files[0].Patch != patch {
						t.Fatal("diff body changed")
					}
					if _, err := d.service.database.db.ExecContext(t.Context(), "UPDATE attempt_diffs SET object_key='',object_sha256='',object_bytes=0,content_bytes=0 WHERE id=?", receipt.DiffID); err != nil {
						t.Fatal(err)
					}
					if _, err := d.service.database.db.ExecContext(t.Context(), "INSERT INTO attempt_diff_files(diff_id,position,path,old_path,status,additions,deletions,binary,patch,truncated,denied,conversion_state) VALUES(?,0,'example.go','','modified',0,0,0,?,0,0,2)", receipt.DiffID, patch); err != nil {
						t.Fatal(err)
					}
					legacy := d.get(t, "")
					requireNativeStatus(t, legacy, http.StatusOK)
					for _, family := range storageConversionFamilies {
						if family.name == "diff_files" {
							if err := d.service.convertObjectPayloadFamily(d.service.contentContext(t.Context()), family, d.service.config.now()); err != nil {
								t.Fatal(err)
							}
						}
					}
					converted := d.get(t, "")
					requireNativeStatus(t, converted, http.StatusOK)
					if legacy.Body.String() != converted.Body.String() {
						t.Fatal("diff backfill changed reads")
					}
				})
			}
		})
	}
}

func TestPayloadPreparationPreservesMutationIDs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(*testing.T) (*Service, tracker.OrganizationID, func(context.Context, *sql.Tx, time.Time) (string, error))
	}{
		{
			name: "attempt diff",
			setup: func(t *testing.T) (*Service, tracker.OrganizationID, func(context.Context, *sql.Tx, time.Time) (string, error)) {
				f := newAttemptDiffFixture(t)
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
				request := f.request(1, tracker.AttemptDiffFile{Path: "example.go", Status: tracker.DiffStatusModified, Patch: "+example"})
				return f.service, scope.organization, func(ctx context.Context, tx *sql.Tx, now time.Time) (string, error) {
					_, err := storeAttemptDiff(ctx, tx, scope, f.attempt, string(f.issue.WorkItemID), request, now)
					var hydration *claimHydration
					if err != nil && !errors.As(err, &hydration) {
						return "", err
					}
					var id string
					readErr := tx.QueryRowContext(ctx, "SELECT id FROM attempt_diffs WHERE attempt_id=?", f.attempt).Scan(&id)
					return id, errors.Join(err, readErr)
				}
			},
		},
		{
			name: "conversation audience",
			setup: func(t *testing.T) (*Service, tracker.OrganizationID, func(context.Context, *sql.Tx, time.Time) (string, error)) {
				f := newConversationFixture(t)
				record := f.create(t, f.record(f.owner, "Audience", f.service.config.now()))
				return f.service, f.organization, func(ctx context.Context, tx *sql.Tx, now time.Time) (string, error) {
					if err := f.store.recordAudience(ctx, tx, record.ID, f.owner, conversation.VisibilityPrivate, conversation.VisibilityShared, now); err != nil {
						return "", err
					}
					var id string
					err := tx.QueryRowContext(ctx, "SELECT id FROM conversation_audience_events WHERE conversation_id=?", record.ID).Scan(&id)
					return id, err
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, organization, mutate := test.setup(t)
			ctx := service.contentContext(t.Context())
			var firstID, key string
			passes := 0
			if err := service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
				passes++
				id, err := mutate(ctx, tx, now)
				if id != "" {
					if firstID == "" {
						firstID = id
					} else if id != firstID {
						return fmt.Errorf("payload preparation changed mutation ID from %s to %s", firstID, id)
					}
				}
				if err != nil {
					return err
				}
				key, err = storedPayloadKey(ctx, string(organization), []byte(id))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if passes < 2 || firstID == "" {
				t.Fatalf("payload preparation passes=%d ID=%q", passes, firstID)
			}
			raw, err := readStoredPayload(ctx, key)
			if err != nil || string(raw) != firstID {
				t.Fatalf("archived mutation ID=%q want=%q error=%v", raw, firstID, err)
			}
		})
	}
}

func TestPayloadPreparationRechecksLease(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	var seconds atomic.Int64
	seconds.Store(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC).Unix())
	var advanced atomic.Bool
	f := claimedAttemptDiffFixture(t, false, Config{now: func() time.Time { return time.Unix(seconds.Load(), 0).UTC() }, Content: func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if request.Write && advanced.CompareAndSwap(false, true) {
			seconds.Add(3600)
		}
		return objects.call(ctx, request)
	}})
	advanced.Store(false)
	event := nativeStartedEvent(f.lease)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event)
	if response.Code < 400 {
		t.Fatalf("expired lease accepted after payload upload: %d", response.Code)
	}
	var count int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired writer stored %d attempts: %v", count, err)
	}
}

func TestPayloadBackfillResumesAfterStorageFailure(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	var fail atomic.Bool
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Content: func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if request.Write && fail.Load() {
			return nil, errors.New("example object write failure")
		}
		return objects.call(ctx, request)
	}}))
	version := f.publish(t, "backfill-version", "")
	ctx := f.service.contentContext(t.Context())
	var first tracker.ChangeReview
	if err := f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		for i := range storageConversionRows + 2 {
			review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: version.ID, Decision: "commented", Body: strings.Repeat("x", 1024), CreatedAt: now.UTC()}
			if i == 0 {
				first = review
			}
			if err := seedLegacyChangeEvidence(ctx, tx, f.change.ID, version.ID, "review", review.ID, review); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE change_evidence SET conversion_state=2 WHERE change_id=?", f.change.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var family storageConversionFamily
	for _, candidate := range storageConversionFamilies {
		if candidate.name == "evidence" {
			family = candidate
		}
	}
	fail.Store(true)
	if err := f.service.convertObjectPayloadFamily(ctx, family, f.service.config.now()); err == nil {
		t.Fatal("failed upload was accepted")
	}
	var converted int
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM change_evidence WHERE payload_object_key<>''").Scan(&converted); err != nil || converted != 0 {
		t.Fatalf("failed upload cleared %d payloads: %v", converted, err)
	}
	fail.Store(false)
	if err := f.service.convertObjectPayloadFamily(ctx, family, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM change_evidence WHERE payload_object_key<>''").Scan(&converted); err != nil || converted != storageConversionRows {
		t.Fatalf("batch converted %d rows: %v", converted, err)
	}
	if err := f.service.convertObjectPayloadFamily(ctx, family, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM change_evidence WHERE payload_object_key='' OR record_json<>'{}'").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("resumed backfill left %d payloads: %v", pending, err)
	}
	var complete bool
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT complete FROM storage_conversion_progress WHERE family='evidence' AND aggregate_id=?", f.change.ID).Scan(&complete); err != nil || !complete {
		t.Fatalf("conversion completion=%t error=%v", complete, err)
	}
	var key string
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT payload_object_key FROM change_evidence WHERE source_key=?", first.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	var restored tracker.ChangeReview
	if err := decodeStorageJSON(ctx, []byte(key), &restored); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var legacy tracker.ChangeReview
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, legacy) {
		t.Fatal("evidence backfill changed full reads")
	}
}

func seedLegacyChangeEvidence(ctx context.Context, tx *sql.Tx, change, version, kind, source string, value any) error {
	raw, err := marshalNative(value)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO change_evidence(change_id,version_id,kind,source_key,record_json) VALUES(?,NULLIF(?,''),?,NULLIF(?,''),?)", change, version, kind, source, raw)
	if err != nil {
		return err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return err
	}
	return projectChangeColumns(ctx, tx, "change_evidence", sequence, raw)
}

func TestRunHistoryWritesOnlyObjectReferences(t *testing.T) {
	t.Parallel()
	for _, ordered := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordered=%t", ordered), func(t *testing.T) {
			objects := &changeObjectFixture{}
			f := claimedAttemptDiffFixture(t, false, Config{Content: objects.call})
			sequence := int64(0)
			if ordered {
				sequence = 1
			}
			data := tracker.NativeRunData{Sequence: sequence, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy}
			if ordered {
				data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}
			}
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "history-started"}, Type: "run.started", SchemaVersion: 1, Data: data}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
			if ordered {
				data.Sequence++
			}
			data.Outcome = "succeeded"
			if ordered {
				data.CompletionBody = strings.Repeat("history detail", 1024)
			}
			event = tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "history-finished"}, Type: "run.finished", SchemaVersion: 1, Data: data}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
			var raw, key string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json,payload_object_key FROM collaboration_events WHERE work_item_id=? AND type='run.finished' ORDER BY sequence DESC LIMIT 1", f.issue.WorkItemID).Scan(&raw, &key); err != nil {
				t.Fatal(err)
			}
			if raw != "{}" || !strings.HasPrefix(key, "orgs/") {
				t.Errorf("run history retained an inline payload (%d bytes)", len(raw))
			}
			closed, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			if err := closed.Close(); err != nil {
				t.Fatal(err)
			}
			ctx := f.service.freshRunDetailContext(t.Context())
			ctx.Value(changeContentKey{}).(*changeContent).query = closed
			var restored tracker.CollaborationData
			if err := decodeRunHistory(ctx, f.service.database.reader, key, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.Run == nil || restored.Run.CompletionBody != data.CompletionBody || restored.Run.AttemptID != data.AttemptID || restored.Run.Outcome != data.Outcome {
				t.Fatal("object-backed history lost full run detail")
			}
		})
	}
	t.Run("missing storage context", func(t *testing.T) {
		f := newDefaultNativeFixture(t, Config{})
		issue := f.create(t, "history storage required")
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
		err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, now time.Time) error {
			return appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: "history detail"}}, now)
		})
		if err == nil {
			t.Fatal("history write without storage context persisted an inline payload")
		}
	})
}

func TestAnalyticsPayloadCompletionUsesScopedPendingIndex(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	plan := strings.Join(explainQueryPlan(t, f.service.database.reader, analyticsPayloadPendingSQL, []any{f.project.OrganizationID, f.project.ID}), "\n")
	if !strings.Contains(plan, "analytics_attempt_payload_pending_scope (organization_id=? AND project_id=?)") {
		t.Fatalf("completion can scan history or other tenants' pending rows: %s", plan)
	}
}

func TestAnalyticsPayloadBackfillResumes(t *testing.T) {
	t.Parallel()
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprintf("uncertain=%t", uncertain), func(t *testing.T) {
			objects := &changeObjectFixture{}
			var fail atomic.Bool
			f := newDefaultNativeFixture(t, Config{Content: func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
				if request.Write && fail.Load() {
					if uncertain {
						if _, err := objects.call(ctx, request); err != nil {
							return nil, err
						}
					}
					return nil, errors.New("example upload interrupted")
				}
				return objects.call(ctx, request)
			}})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			ctx := f.service.freshRunDetailContext(t.Context())
			now := f.service.config.now()
			raw := `{"work_item_id":"example-item","status":"failed","identity":{"role":"merge"},"runtime":{"landing":{"landed":false,"refusal_kind":"conflict"}},"completion_body":"historical detail"}`
			if err := f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
				for index := range storageConversionRows + 2 {
					if _, err := tx.ExecContext(ctx, "INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version) VALUES(?,?,0,'attempt',?,1,?,?)", f.project.OrganizationID, f.project.ID, fmt.Sprintf("legacy-%03d", index), raw, map[bool]any{false: nil, true: 1}[index%2 == 0]); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var family storageConversionFamily
			for _, candidate := range storageConversionFamilies {
				if candidate.name == "analytics" {
					family = candidate
				}
			}
			fail.Store(true)
			if err := f.service.convertObjectPayloadFamily(ctx, family, now); err == nil {
				t.Fatal("interrupted analytics upload was accepted")
			}
			var retained int
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM analytics_hourly_rollups WHERE metric='attempt' AND data_json=?", raw).Scan(&retained); err != nil || retained != storageConversionRows+2 {
				t.Fatalf("failed upload retained %d payloads: %v", retained, err)
			}
			fail.Store(false)
			for _, want := range []int{storageConversionRows, storageConversionRows + 2} {
				batchContext := f.service.freshRunDetailContext(t.Context())
				if err := f.service.convertObjectPayloadFamily(batchContext, family, now); err != nil {
					t.Fatal(err)
				}
				var converted int
				if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM analytics_hourly_rollups WHERE metric='attempt' AND data_json='{}' AND payload_object_key<>''").Scan(&converted); err != nil || converted != want {
					t.Fatalf("batch converted %d payloads, want %d: %v", converted, want, err)
				}
			}
			var complete bool
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT complete FROM storage_conversion_progress WHERE family='analytics' AND organization_id=? AND project_id=?", f.project.OrganizationID, f.project.ID).Scan(&complete); err != nil || !complete {
				t.Fatalf("analytics conversion completion=%t: %v", complete, err)
			}
			var item, status, role string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT work_item_id,status,attempt_role FROM analytics_hourly_rollups WHERE dimension='legacy-001'").Scan(&item, &status, &role); err != nil || item != "example-item" || status != "failed" || role != "merge" {
				t.Fatalf("backfill lost typed analytics facts: item=%q status=%q role=%q error=%v", item, status, role, err)
			}
			var key string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT payload_object_key FROM analytics_hourly_rollups WHERE metric='attempt' LIMIT 1").Scan(&key); err != nil {
				t.Fatal(err)
			}
			restored, err := readStoredPayload(ctx, key)
			if err != nil || string(restored) != raw {
				t.Fatalf("backfill changed historical analytics detail: %v", err)
			}
			objects.mu.Lock()
			writes, unique := objects.writes, len(objects.objects)
			objects.mu.Unlock()
			if unique != 1 {
				t.Fatalf("retry produced %d objects for identical content", unique)
			}
			if err := f.service.convertObjectPayloadFamily(f.service.freshRunDetailContext(t.Context()), family, now); err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			after := objects.writes
			objects.mu.Unlock()
			if after != writes {
				t.Fatal("completed analytics conversion uploaded again")
			}
		})
	}
}

func TestArchivedBlockerHistoryAuthorizesDependencyRecovery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		projected    bool
		postRunEdits int
	}{
		{projected: false},
		{projected: true},
		{projected: false, postRunEdits: 10},
		{projected: false, postRunEdits: 1000},
		{projected: true, postRunEdits: 10},
		{projected: true, postRunEdits: 1000},
	} {
		t.Run(fmt.Sprintf("projected=%t/edits=%d", test.projected, test.postRunEdits), func(t *testing.T) {
			objects := &changeObjectFixture{}
			f := claimedAttemptDiffFixture(t, false, Config{Content: objects.call})
			dependency := f.create(t, "prerequisite")
			states := nativeFixtureStates()
			states[0].Transitions = append(states[0].Transitions, "Blocked")
			states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
			if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, now time.Time) error {
				return applyNativeProjectStates(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, states, now)
			}); err != nil {
				t.Fatal(err)
			}
			for index := range 5 {
				ctx := f.service.freshRunDetailContext(t.Context())
				data := tracker.CollaborationData{Revision: f.issue.Revision, ReasonDetail: strings.Repeat("x", attachment.MaxArchiveBytes/4) + strconv.Itoa(index), Run: &tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt_old%d", index), FencingToken: 1}}
				if err := f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
					return appendLegacyNativeHistory(ctx, tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID}}, string(f.issue.WorkItemID), "run.finished", data, now)
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.start(t)
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			reference := fmt.Sprintf("%s#%d", f.project.ID, dependency.Number)
			finish := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "finish-blocked"}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded",
					Disposition: &tracker.NativeDisposition{Status: workpad.StatusBlocked, Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: reference, Owner: workpad.BlockerOwnerOrchestrator, Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Identifier: reference, States: []string{"open"}}}}}}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, finish), http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &issue)
			if issue.Revision <= f.issue.Revision || !slices.Contains(issue.Dependencies, dependency.WorkItemID) {
				t.Fatal("completion did not add the dependency and advance revision")
			}
			transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "blocked", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ExpectedRevision: issue.Revision, State: "Blocked", Reason: "worker_progress"}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, transition)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/release", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependency.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "dependency-done"}, ExpectedRevision: dependency.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
			var inline, key string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json,payload_object_key FROM collaboration_events WHERE work_item_id=? AND type='run.finished' ORDER BY sequence DESC LIMIT 1", issue.WorkItemID).Scan(&inline, &key); err != nil {
				t.Fatal(err)
			}
			if inline != "{}" || key == "" {
				t.Fatal("run completion was not archived")
			}
			if !test.projected {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_events SET page_projection_ready=NULL,event_revision=NULL WHERE work_item_id=? AND type='run.finished'", issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID}}
			if test.postRunEdits > 0 {
				keys := make([]string, test.postRunEdits)
				for index := range keys {
					data := tracker.CollaborationData{Revision: issue.Revision + tracker.Revision(index) + 1, Fields: []string{"body"}}
					raw, err := json.Marshal(data)
					if err != nil {
						t.Fatal(err)
					}
					ref, err := prepareChangeContent(f.service.freshRunDetailContext(t.Context()), string(scope.organization), raw)
					if err != nil {
						t.Fatal(err)
					}
					keys[index] = ref.Key
				}
				ctx := f.service.freshRunDetailContext(t.Context())
				if err := f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
					for index, key := range keys {
						var sequence int64
						if err := tx.QueryRowContext(ctx, "UPDATE issues SET event_sequence=event_sequence+1 WHERE native_id=? RETURNING event_sequence", issue.WorkItemID).Scan(&sequence); err != nil {
							return err
						}
						_, err := tx.ExecContext(ctx, `INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,payload_object_key,recorded_at,page_projection_ready,event_revision,actor_kind)
VALUES(?,?,?,?,?,'issue.edited',1,'{"kind":"human","principal_id":"example-editor"}','{}',?,?,1,?,'human')`, newNativeID("evt"), scope.organization, scope.project, issue.WorkItemID, sequence, key, formatHubTime(now), issue.Revision+tracker.Revision(index)+1)
						if err != nil {
							return err
						}
					}
					revision := issue.Revision + tracker.Revision(test.postRunEdits) + 1
					runnerScope := scope
					runnerScope.sourceActor = &tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}
					if err := appendNativeHistory(ctx, tx, runnerScope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{Revision: revision, FromState: "Todo", ToState: "Blocked", Reason: "worker_progress"}, now); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, "UPDATE issues SET revision=? WHERE native_id=?", revision, issue.WorkItemID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				issue.Revision += tracker.Revision(test.postRunEdits) + 1
			}
			attempt, err := readNativeAttemptRecord(f.service.freshRunDetailContext(t.Context()), f.service.database.reader, scope, string(issue.WorkItemID), f.attempt, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			beforeHistory := objects.reads
			objects.mu.Unlock()
			query := &runtimeReadQuery{nativeQueryer: f.service.database.reader}
			history, err := readNativeBlockerHistory(f.service.freshRunDetailContext(t.Context()), query, scope, issue, attempt)
			if err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			historyObjects := objects.reads - beforeHistory
			objects.mu.Unlock()
			historyRows := 0
			for index, statement := range query.statements {
				if !strings.Contains(statement, "FROM collaboration_events") {
					continue
				}
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM ("+statement+")", query.arguments[index]...).Scan(&count); err != nil {
					t.Fatal(err)
				}
				historyRows += count
			}
			t.Logf("post-run edits=%d: history rows=%d objects=%d", test.postRunEdits, historyRows, historyObjects)
			if historyRows > 4 || historyObjects > 1 {
				t.Errorf("post-run edits=%d: recovery read %d history rows and %d objects", test.postRunEdits, historyRows, historyObjects)
			}
			_, _, valid := tracker.RecordedNativeBlockers(issue, []tracker.NativeAttempt{attempt}, history)
			if valid != (test.postRunEdits == 0) {
				t.Errorf("post-run edits=%d: recovery authorized=%t", test.postRunEdits, valid)
			}
			transition = tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "recover", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "dependency_ready", ReasonDetail: "recorded blocker recovery", BlockerAttemptID: f.attempt, PolicyID: f.policy}
			if test.postRunEdits > 0 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, transition), http.StatusUnprocessableEntity)
				return
			}
			for _, changed := range []string{"attempt sequence", "issue revision"} {
				t.Run("stale "+changed, func(t *testing.T) {
					type recoveryRequestKey struct{}
					var injected atomic.Bool
					f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
						if ctx.Value(recoveryRequestKey{}) != nil && !request.Write && request.Reference.Key == key && injected.CompareAndSwap(false, true) {
							statement := "UPDATE native_attempts SET sequence=sequence+1 WHERE id=?"
							id := f.attempt
							if changed == "issue revision" {
								statement, id = "UPDATE issues SET revision=revision+1 WHERE native_id=?", string(issue.WorkItemID)
							}
							if _, err := f.service.database.db.ExecContext(ctx, statement, id); err != nil {
								return nil, err
							}
						}
						return objects.call(ctx, request)
					}
					request := transition
					request.IdempotencyKey += "-" + changed
					response := httptest.NewRecorder()
					ctx := context.WithValue(t.Context(), recoveryRequestKey{}, true)
					f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/workflow", f.worker, request).WithContext(ctx))
					f.service.config.Content = objects.call
					if !injected.Load() {
						t.Fatal("recovery did not hydrate archived completion evidence")
					}
					statement := "UPDATE native_attempts SET sequence=sequence-1 WHERE id=?"
					id := f.attempt
					if changed == "issue revision" {
						statement, id = "UPDATE issues SET revision=revision-1 WHERE native_id=?", string(issue.WorkItemID)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), statement, id); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, response, http.StatusConflict)
				})
			}
			objects.mu.Lock()
			readsBefore := objects.reads
			objects.mu.Unlock()
			writerHistoryReads := 0
			ctx := withRequestTiming(t.Context(), &requestTiming{writerTrace: func(query string, _ []driver.NamedValue) {
				if strings.Contains(query, "FROM collaboration_events") && strings.Contains(query, "event_revision") {
					writerHistoryReads++
				}
			}})
			response = httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/workflow", f.worker, transition).WithContext(ctx))
			requireNativeStatus(t, response, http.StatusOK)
			if writerHistoryReads != 0 {
				t.Errorf("recovery read blocker history %d times on the writer", writerHistoryReads)
			}
			objects.mu.Lock()
			reads := objects.reads - readsBefore
			objects.mu.Unlock()
			if reads > 12 {
				t.Errorf("recovery read %d objects, including unrelated issue history", reads)
			}
			decodeHubResponse(t, response, &issue)
			if issue.State != "Todo" {
				t.Fatalf("recovered state = %q", issue.State)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, transition), http.StatusOK)
		})
	}
}

func TestAttemptPayloadStorageUnavailableAPI(t *testing.T) {
	t.Parallel()
	for _, storage := range []string{"unconfigured", "outage", "untyped outage"} {
		t.Run(storage, func(t *testing.T) {
			f := claimedAttemptDiffFixture(t, false, Config{})
			f.start(t)
			if storage == "unconfigured" {
				f.service.config.Content = nil
			} else {
				f.service.config.Content = func(context.Context, entryauthority.ContentRequest) ([]byte, error) {
					if storage == "untyped outage" {
						return nil, errors.New("private-storage-endpoint")
					}
					return nil, fmt.Errorf("private-storage-endpoint: %w", artifact.ErrStorage)
				}
			}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "storage-failure"}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded"}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event)
			requireNativeStatus(t, response, http.StatusServiceUnavailable)
			var failure apiErrorResponse
			decodeHubResponse(t, response, &failure)
			if failure.Code != "storage_unreachable" || !strings.Contains(failure.Message, "storage") || storage == "unconfigured" && !strings.Contains(failure.Message, "configur") {
				t.Errorf("storage failure = %+v", failure)
			}
			if strings.Contains(response.Body.String(), "private-storage-endpoint") {
				t.Fatal("storage failure exposed internal details")
			}
			var sequence int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM native_attempts WHERE id=?", f.attempt).Scan(&sequence); err != nil || sequence != 1 {
				t.Fatalf("failed storage write changed attempt: sequence=%d error=%v", sequence, err)
			}
		})
	}
}

func TestLandingReceiptPayloadReferencesAndConversion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		legacy, different bool
		invalid           string
	}{
		{name: "new write"},
		{name: "legacy matching attempt", legacy: true},
		{name: "legacy different receipt", legacy: true, different: true},
		{name: "missing runtime", invalid: `{}`},
		{name: "missing landing", invalid: `{"runtime":{}}`},
		{name: "null landing", invalid: `{"runtime":{"landing":null}}`},
		{name: "empty landing", invalid: `{"runtime":{"landing":{}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			version := f.publish(t, "landing", "")
			runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			runner.enroll(t)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			receipt := tracker.NativeLandingReceipt{VersionID: version.ID, ChangeID: f.change.ID, MergeSHA: strings.Repeat("c", 40), Landed: true, ObservedAt: version.CreatedAt.Add(time.Minute), Gate: &gate.CommandResult{Command: "go test ./...", Output: strings.Repeat("gate evidence", 8192)}}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			attempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(runner.binding.MachineID), "succeeded", tracker.NativeRunData{}, receipt.ObservedAt, -1)
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			ctx := f.service.freshRunDetailContext(t.Context())
			err = f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
				return persistAttemptLanding(ctx, tx, attempt, receipt)
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.legacy {
				if test.different {
					receipt.MergeSHA = strings.Repeat("d", 40)
					receipt.Gate.Output = "historical receipt"
				}
				raw, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(ctx, "UPDATE change_landing_receipts SET record_json=?,payload_object_key='',merge_sha=NULL WHERE attempt_id=?", string(raw), attempt); err != nil {
					t.Fatal(err)
				}
			}
			before, err := readVersionLanding(ctx, f.service.database.reader, version.ID)
			if err != nil || !reflect.DeepEqual(before, receipt) {
				t.Fatalf("receipt before conversion differs: %v", err)
			}
			if test.legacy {
				if err := f.service.convertLegacyStorage(ctx, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			var inline, merge string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT record_json,merge_sha FROM change_landing_receipts WHERE attempt_id=?", attempt).Scan(&inline, &merge); err != nil {
				t.Fatal(err)
			}
			if merge != receipt.MergeSHA {
				t.Fatal("conversion lost the typed merge SHA")
			}
			if inline != "{}" {
				t.Fatalf("landing receipt retained %d inline payload bytes", len(inline))
			}
			var source, derived string
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT a.payload_object_key,r.payload_object_key FROM native_attempts a JOIN change_landing_receipts r ON r.attempt_id=a.id WHERE a.id=?", attempt).Scan(&source, &derived); err != nil {
				t.Fatal(err)
			}
			if source == "" || derived == "" || (source != derived) != test.different {
				t.Fatal("landing receipt did not preserve or share the expected attempt snapshot")
			}
			if test.invalid != "" {
				ref, err := prepareChangeContent(ctx, string(f.project.OrganizationID), []byte(test.invalid))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(ctx, "UPDATE change_landing_receipts SET payload_object_key=? WHERE attempt_id=?", ref.Key, attempt); err != nil {
					t.Fatal(err)
				}
			}
			after, err := readVersionLanding(f.service.freshRunDetailContext(t.Context()), f.service.database.reader, version.ID)
			if test.invalid != "" {
				var decodeErr *landingReceiptDecodeError
				if !errors.As(err, &decodeErr) || !errors.Is(err, artifact.ErrIntegrity) {
					t.Errorf("invalid archived landing receipt returned %v, want typed integrity error", err)
				}
			} else if err != nil || !reflect.DeepEqual(after, receipt) {
				t.Fatalf("receipt after conversion differs: %v", err)
			}
			candidates, partial, err := readValidationCandidates(f.service.freshRunDetailContext(t.Context()), f.service.database.reader, scope, string(f.issue.WorkItemID), f.change.ID, attempt, gate.ScheduledEvidence{Repository: validationRepository(version.Repository), HeadSHA: receipt.MergeSHA})
			if test.invalid != "" {
				var decodeErr *landingReceiptDecodeError
				if !errors.As(err, &decodeErr) || !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("validation accepted invalid archived landing receipt: %v", err)
				}
				return
			}
			if err != nil || partial || len(candidates) != 1 {
				t.Fatalf("validation candidates: count=%d partial=%t error=%v", len(candidates), partial, err)
			}
			candidate := candidates[0]
			if candidate.IntegratedHeadSHA != receipt.MergeSHA || candidate.IntegratedSource != "same_commit" || !candidate.ObservedAt.Equal(receipt.ObservedAt) {
				t.Fatalf("validation lost landing provenance: %+v", candidate)
			}
			local := *receipt.Gate
			local.Output = ""
			local.Command = "go test [redacted]"
			if !reflect.DeepEqual(candidate.Local, &local) {
				t.Fatalf("validation lost landing gate: %+v", candidate.Local)
			}
		})
	}
}
