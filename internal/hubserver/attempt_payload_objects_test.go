package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestEvidencePayloadSizeAndReadEquivalence(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	f := newChangeFixture(t, newPostgresNativeService(t, Config{Content: objects.call}))
	version := f.publish(t, "payload-version", "")
	ctx := f.service.contentContext(t.Context())
	var inlineBytes int
	for _, size := range []int{32, 256 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: version.ID, Decision: "commented", Body: strings.Repeat("x", size), CreatedAt: f.service.config.now().UTC()}
			row := payloadEvidenceFixture(t, f.service, f.project.OrganizationID, f.change.ID, version.ID, "review", review.ID, review)
			stored := len(row.PayloadObjectKey) + len(row.PayloadSHA256)
			if inlineBytes != 0 && stored != inlineBytes {
				t.Errorf("inline payload grew from %d to %d bytes", inlineBytes, stored)
			}
			inlineBytes = stored
			var got, legacy tracker.ChangeReview
			archived, err := eventstore.NewContent(f.service.runDetailStore()).Evidence(ctx, row)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(archived, &got); err != nil {
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
			detail := f.detail(t)
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
			fetches := map[attachment.ArchiveReference]int{}
			sourceWrites := 0
			contentCall := func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
				if ctx.Value(analyticsPayloadReadKey{}) == true {
					objects.mu.Lock()
					if request.Write {
						sourceWrites++
					} else {
						fetches[request.Reference]++
					}
					objects.mu.Unlock()
				}
				return objects.call(ctx, request)
			}
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			f := claimedAttemptDiffFixture(t, false, Config{Content: contentCall, now: func() time.Time { return now }})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			f.service.outbox.stop()
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			data := tracker.NativeRunData{Sequence: 1, MachineID: f.lease.MachineID, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Handoff: nativeTestCheckpoint()}
			started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "payload-start"}, Type: "run.started", SchemaVersion: 1, Data: data}
			started.Data.Handoff = nil
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, started), http.StatusOK)
			now = now.Add(time.Minute)
			data.Sequence++
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
			payloadAssertAttemptReferences(t, f.service, f.project.OrganizationID, f.attempt)
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
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.worker), "", "")
			if err != nil {
				t.Fatal(err)
			}
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
					request := tracker.PublishChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{RunID: f.run, AttemptID: f.attempt, PolicyID: authority.policy}}
					request.LeaseID, request.FencingToken = f.lease.ID, authority.fence
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
					_, err := f.service.prepareChangeVersionRun(t.Context(), scope, tracker.ChangeRequest{WorkItemID: f.issue.WorkItemID}, request)
					if (err == nil) != authority.allowed {
						t.Fatalf("source authority allowed=%v error=%v", authority.allowed, err)
					}
				})
			}
			objects.mu.Lock()
			before := objects.reads
			objects.mu.Unlock()
			for _, suffix := range []string{"/attempts?view=recovery", "?view=recovery&detail=summary"} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+suffix, f.token, nil), http.StatusOK)
			}
			objects.mu.Lock()
			recoveryReads := objects.reads - before
			objects.mu.Unlock()
			if recoveryReads != 0 {
				t.Fatalf("typed recovery fetched %d objects", recoveryReads)
			}
			ctx := f.service.runDetailContext(t.Context())
			scope := analyticsTestScope(t, f.nativeFixture)
			admin := hostedPostgresAdmin(t, f.service)
			var storedEvent eventstore.Event
			var sourceAttempt leasestore.Attempt
			var sourceArchive artifactstore.ArchiveObject
			if err := admin.QueryRowContext(ctx, `SELECT payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND attempt_id=$4 AND type='run.finished' ORDER BY sequence DESC LIMIT 1`, scope.organization, scope.project, f.issue.WorkItemID, f.attempt).Scan(&storedEvent.PayloadObjectKey, &storedEvent.PayloadSHA256, &storedEvent.PayloadBytes, &storedEvent.PayloadContentBytes); err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				sourceAttempt, err = r.Attempt(ctx, f.attempt)
				if err != nil {
					return err
				}
				sourceArchive, err = r.ArchiveObject(ctx, "archive_"+sourceAttempt.PayloadObjectKey[strings.LastIndex(sourceAttempt.PayloadObjectKey, "/")+1:])
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if sourceAttempt.PayloadObjectKey == "" || sourceArchive.Reference.Key != sourceAttempt.PayloadObjectKey || sourceArchive.Reference.SHA256 == "" {
				t.Fatalf("stored attempt source reference=%+v archive=%+v", sourceAttempt, sourceArchive)
			}
			expected := eventstore.Reference(storedEvent.PayloadObjectKey, storedEvent.PayloadSHA256, storedEvent.PayloadBytes, storedEvent.PayloadContentBytes)
			selectedCtx := context.WithValue(ctx, analyticsPayloadReadKey{}, true)
			population, _, err := f.service.readNativeAnalyticsAttemptPopulationTenant(selectedCtx, scope, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, "", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			writes := sourceWrites
			reads := make(map[attachment.ArchiveReference]int, len(fetches))
			for ref, count := range fetches {
				reads[ref] = count
			}
			objects.mu.Unlock()
			if writes != 0 {
				t.Fatal("analytics duplicated the source payload")
			}
			if len(reads) != 1 || reads[expected] != 1 {
				t.Fatalf("analytics fetched outside the exact selected source reference: reads=%+v selected=%+v", reads, expected)
			}
			if len(population.Items) != 1 || population.Items[0].AttemptID != f.attempt {
				t.Fatalf("analytics did not retain the selected attempt: %+v", population)
			}
			if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				after, err := r.Attempt(ctx, f.attempt)
				if err != nil {
					return err
				}
				archive, err := r.ArchiveObject(ctx, sourceArchive.ID)
				if err != nil {
					return err
				}
				if after.PayloadObjectKey != sourceAttempt.PayloadObjectKey || archive.Reference != sourceArchive.Reference {
					t.Fatal("analytics changed the exact stored attempt key or hash")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(population)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte(data.CompletionBody)) && data.CompletionBody != "" {
				t.Fatal("analytics retained an inline attempt payload copy")
			}

			var artifactKey string
			if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.Attempt(ctx, f.attempt)
				if err != nil {
					return err
				}
				if row.ArtifactsObjectKey == nil {
					return artifact.ErrIntegrity
				}
				artifactKey = *row.ArtifactsObjectKey
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			artifactContent := bytes.Clone(objects.objects[artifactKey])
			objects.mu.Unlock()
			for _, failure := range []string{"missing", "corrupt"} {
				t.Run("retained artifact/"+failure, func(t *testing.T) {
					objects.mu.Lock()
					if failure == "missing" {
						delete(objects.objects, artifactKey)
					} else {
						objects.objects[artifactKey] = []byte("corrupt")
					}
					objects.mu.Unlock()
					defer func() { objects.mu.Lock(); objects.objects[artifactKey] = artifactContent; objects.mu.Unlock() }()
					_, err := f.service.readNativeAttemptTenant(ctx, scope, string(f.issue.WorkItemID), f.attempt)
					if !errors.Is(err, artifact.ErrIntegrity) {
						t.Fatalf("retained artifact %s silently fell back: %v", failure, err)
					}
					var unavailable *nativeArchiveError
					if !errors.As(err, &unavailable) || unavailable.Archive == nil || unavailable.Archive.Availability != "unavailable" && unavailable.Archive.Availability != "invalid" {
						t.Fatalf("retained artifact %s lost archive refusal: %v", failure, err)
					}
				})
			}

			payloadAssertHistoricalAttemptReads(t, data, artifacts)
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
					ctx := f.service.contentContext(t.Context())
					row, err := f.service.readChangeVersionRecord(ctx, f.project.OrganizationID, f.change.ID, version.ID)
					if err != nil {
						t.Fatal(err)
					}
					if row.PayloadObjectKey == "" || row.SourceObjectKey == "" || len(row.PayloadSHA256) != 64 || len(row.SourceSHA256) != 64 {
						t.Fatal("version/source content remained inline or lost its immutable reference")
					}
					content := eventstore.NewContent(f.service.runDetailStore())
					restored, err := content.Version(ctx, row)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(restored, version) {
						t.Fatal("version read differs from publication")
					}
					got, err := content.SourceBundle(ctx, f.project.OrganizationID, row, *version.Source)
					if err != nil {
						t.Fatal(err)
					}
					if string(got) != string(bundle) {
						t.Fatal("source object changed bundle bytes")
					}
					payloadAssertHistoricalVersion(t, f, version, bundle)
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
					if err := d.service.config.TenantRepository.ReadTenant(t.Context(), d.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.AttemptDiff(t.Context(), d.project.ID, d.attempt, tracker.DiffGeneration{Source: "attempt", Seq: 1})
						if err != nil {
							return err
						}
						archive, err := r.ArchiveObject(t.Context(), row.ArchiveID)
						if err != nil {
							return err
						}
						if row.ID != receipt.DiffID || archive.Reference.Key == "" || row.PatchBytes != int64(len(patch)) {
							t.Fatal("diff content remained inline or lost its archive reference")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					response = d.get(t, "")
					requireNativeStatus(t, response, http.StatusOK)
					var diff tracker.AttemptDiff
					decodeHubResponse(t, response, &diff)
					if len(diff.Files) != 1 || diff.Files[0].Patch != patch {
						t.Fatal("diff body changed")
					}
					payloadAssertHistoricalDiff(t, patch)
				})
			}
		})
	}
}

func TestPayloadPreparationPreservesMutationIDs(t *testing.T) {
	t.Parallel()
	objects := &conversationContentFixture{}
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ControlQueueSize: 4, ArchiveContents: objects})
	dispatches := 0
	f.service.config.ConversationDispatch = conversationDispatchRequestFunc(func(_ context.Context, _ tenantstore.Writer, project tracker.ProjectID, item string, _ leasestore.Fence, _ time.Time) error {
		if project != f.project.ID || item == "" {
			t.Fatal("prepared mutation lost its issue scope")
		}
		dispatches++
		return nil
	})
	previousID := ""
	for _, title := range []string{"Example first issue", "Example second issue"} {
		record := f.create(t, f.token, map[string]any{"title": title}).Conversation
		response := f.link(t, f.token, record.ID, "payload-mutation", true, title)
		requireNativeStatus(t, response, http.StatusOK)
		var firstID string
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT id FROM conversation_audience_events WHERE organization_id=$1 AND conversation_id=$2 AND actor_principal_id=$3 AND from_visibility='private' AND to_visibility='shared'`, f.project.OrganizationID, record.ID, f.ownerID).Scan(&firstID); err != nil {
			t.Fatal(err)
		}
		if firstID == "" || firstID == previousID {
			t.Fatal("prepared mutations reused or omitted an audience ID")
		}
		publications := objects.publications
		replay := f.link(t, f.token, record.ID, "payload-mutation", true, title)
		requireNativeStatus(t, replay, http.StatusOK)
		if response.Body.String() != replay.Body.String() {
			t.Fatal("archived mutation receipt changed IDs on replay")
		}
		if objects.publications != publications {
			t.Fatal("replaying a prepared mutation republished its payload")
		}
		var count int
		var replayID string
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*),min(id) FROM conversation_audience_events WHERE organization_id=$1 AND conversation_id=$2`, f.project.OrganizationID, record.ID).Scan(&count, &replayID); err != nil {
			t.Fatal(err)
		}
		if count != 1 || replayID != firstID {
			t.Fatalf("payload preparation changed mutation ID from %s to %s across %d rows", firstID, replayID, count)
		}
		previousID = firstID
	}
	if dispatches != 2 {
		t.Fatalf("prepared mutation dispatches=%d want=2", dispatches)
	}
}

func TestPayloadPreparationRechecksLease(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	var seconds atomic.Int64
	seconds.Store(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC).Unix())
	var advanced atomic.Bool
	f := claimedAttemptDiffFixture(t, false, Config{now: func() time.Time { return time.Unix(seconds.Load(), 0).UTC() }, Content: objects.call, RunDetails: &memoryRunDetails{afterStore: func(attachment.ArchiveReference) {
		if advanced.CompareAndSwap(false, true) {
			seconds.Add(3600)
		}
	}}})
	advanced.Store(false)
	event := nativeStartedEvent(f.lease)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event)
	if response.Code < 400 {
		t.Fatalf("expired lease accepted after payload upload: %d", response.Code)
	}
	count := 0
	err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.Attempt(t.Context(), event.Data.AttemptID)
		if err == nil {
			count++
		}
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil || count != 0 {
		t.Fatalf("expired writer stored %d attempts: %v", count, err)
	}
}

func TestRunHistoryWritesOnlyObjectReferences(t *testing.T) {
	t.Parallel()
	for _, ordered := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordered=%t", ordered), func(t *testing.T) {
			var f nativeFixture
			var data tracker.NativeRunData
			if ordered {
				current := claimedAttemptDiffFixture(t, false, Config{})
				f = current.nativeFixture
				data = tracker.NativeRunData{Sequence: 1, LeaseID: current.lease.ID, FencingToken: current.lease.FencingToken, RunID: current.run, AttemptID: current.attempt, PolicyID: current.policy, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}}
				path := f.base + "/work-items/" + string(current.issue.WorkItemID)
				rejected := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "history-unordered-current"}, Type: "run.started", SchemaVersion: 1, Data: data}
				rejected.Data.Sequence = 0
				rejectedResponse := performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", current.worker, rejected)
				requireNativeStatus(t, rejectedResponse, http.StatusUnprocessableEntity)
				var refusal apiErrorResponse
				decodeHubResponse(t, rejectedResponse, &refusal)
				if refusal.Code != "invalid_request" {
					t.Fatalf("current unordered run refusal=%q", refusal.Code)
				}
				event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "history-started"}, Type: "run.started", SchemaVersion: 1, Data: data}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", current.worker, event), http.StatusOK)
				data.Sequence++
				data.Outcome, data.CompletionBody = "succeeded", strings.Repeat("history detail", 1024)
				event = tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "history-finished"}, Type: "run.finished", SchemaVersion: 1, Data: data}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", current.worker, event), http.StatusOK)
			} else {
				f = newDefaultNativeFixture(t, Config{})
				source := coreWorkItemNeutralFixture(t, f.project.OrganizationID, f.project.ID, false)
				data = tracker.NativeRunData{AttemptID: "attempt_history_unordered", RunID: "run_history_unordered", LeaseID: "lease_history_unordered", FencingToken: 1, PolicyID: "policy_history_unordered", Outcome: "succeeded"}
				for _, suffix := range []string{"", "_second"} {
					run := data
					run.AttemptID += suffix
					actor, err := json.Marshal(tracker.Actor{Kind: "runner", PrincipalID: "historical-worker"})
					if err != nil {
						t.Fatal(err)
					}
					payload, err := json.Marshal(tracker.CollaborationData{Revision: 2, Run: &run})
					if err != nil {
						t.Fatal(err)
					}
					item := "wi_core_import" + suffix
					source.add("collaboration_events", map[string]any{"id": "event_history_unordered" + suffix, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item, "sequence": 3, "type": "run.finished", "schema_version": 1, "actor_json": string(actor), "data_json": string(payload), "recorded_at": "2026-10-02T01:00:00Z"})
					source.set("issues", map[string]string{"native_id": item}, map[string]any{"event_sequence": 3})
				}
				count, err := ImportCoreWorkItems(t.Context(), f.service.config.TenantRepository.(*postgres.Repository), CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()})
				if err != nil || count != 2 {
					t.Fatalf("unordered historical run import count=%d error=%v", count, err)
				}
			}
			attempts := []string{data.AttemptID}
			if !ordered {
				attempts = append(attempts, data.AttemptID+"_second")
			}
			for _, attempt := range attempts {
				var row eventstore.Event
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.LatestRunEvent(t.Context(), f.project.ID, attempt)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if row.Type != "run.finished" || !strings.HasPrefix(row.PayloadObjectKey, "orgs/") || row.PayloadBytes <= 0 || row.PayloadContentBytes <= 0 {
					t.Errorf("run history lacks an immutable object reference: %+v", row)
				}
				restored, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), row)
				if err != nil {
					t.Fatal(err)
				}
				if restored.Data.Run == nil || restored.Data.Run.CompletionBody != data.CompletionBody || restored.Data.Run.AttemptID != attempt || restored.Data.Run.Outcome != data.Outcome || restored.Data.Run.Sequence != data.Sequence {
					t.Fatal("object-backed history lost full run detail")
				}
			}
		})
	}
	t.Run("missing storage context", func(t *testing.T) {
		f := newDefaultNativeFixture(t, Config{})
		issue := f.create(t, "history storage required")
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: "event-missing-storage", OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: issue.WorkItemID, AggregateType: "issue", Type: "run.finished", SchemaVersion: 1, RecordedAt: f.service.config.now(), Data: tracker.CollaborationData{Run: &tracker.NativeRunData{CompletionBody: "history detail"}}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = eventstore.NewContent(nil).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
		if !errors.Is(err, artifact.ErrStorage) {
			t.Fatalf("history write without storage context error=%v want storage failure", err)
		}
	})
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
			coordinatorFixtureProject(t, f.service, f.project.OrganizationID, f.project.ID, func(row *projectstore.Record) { row.States = states })
			for index := range 5 {
				data := tracker.CollaborationData{Revision: f.issue.Revision, ReasonDetail: strings.Repeat("x", attachment.MaxArchiveBytes/4) + strconv.Itoa(index), Run: &tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt_old%d", index), FencingToken: 1}}
				appendTestHealthEventAs(t, f.nativeFixture, tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, f.issue, "run.finished", data, f.service.config.now())
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
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.worker), "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
			admin := hostedPostgresAdmin(t, f.service)
			var report eventstore.Event
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				report, err = r.LatestRunEvent(t.Context(), scope.project, f.attempt)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			key := report.PayloadObjectKey
			if key == "" || report.PayloadSHA256 == "" {
				t.Fatal("run completion was not archived")
			}
			if !test.projected {
				if _, err := admin.ExecContext(t.Context(), `UPDATE collaboration_events SET event_revision=0 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND id=$4`, scope.organization, scope.project, issue.WorkItemID, report.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.postRunEdits > 0 {
				issue = payloadSeedRecoveryEdits(t, f.nativeFixture, issue, test.postRunEdits)
			}
			var row corestore.WorkItem
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				row, err = r.WorkItem(t.Context(), scope.project, issue.WorkItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			attemptSnapshot, err := f.service.readNativeRecoveryAttemptSnapshot(t.Context(), scope, string(issue.WorkItemID), row.ID, f.attempt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.hydrateNativeAttemptTenant(t.Context(), attemptSnapshot, f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			probe := &payloadRecoveryRepository{Repository: f.service.config.TenantRepository.(*postgres.Repository)}
			f.service.config.TenantRepository = probe
			readContext := context.WithValue(f.service.freshRunDetailContext(t.Context()), payloadRecoveryProbeKey{}, probe)
			historyObjects := 0
			originalContent := f.service.config.Content
			f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
				if ctx.Value(payloadRecoveryProbeKey{}) == probe && !request.Write && request.Reference.Key != attemptSnapshot.attempt.PayloadObjectKey {
					historyObjects++
				}
				return objects.call(ctx, request)
			}
			readContext = context.WithValue(f.service.freshRunDetailContext(t.Context()), payloadRecoveryProbeKey{}, probe)
			proof := tracker.Transition{Mutation: tracker.Mutation{LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, State: "Todo", BlockerAttemptID: f.attempt, PolicyID: f.policy}
			recovery, proofErr := f.service.prepareRecordedRecoveryTenant(readContext, scope, row, issue, proof)
			f.service.config.Content = originalContent
			if proofErr == nil {
				proofErr = probe.WriteTenant(readContext, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					return f.service.authorizeRecordedRecoveryTenant(readContext, w, scope, *recovery)
				})
			}
			historyRows := probe.rows.Load()
			t.Logf("post-run edits=%d: history rows=%d objects=%d", test.postRunEdits, historyRows, historyObjects)
			if historyRows > 4 || historyObjects > 1 {
				t.Errorf("post-run edits=%d: recovery read %d history rows and %d objects", test.postRunEdits, historyRows, historyObjects)
			}
			valid := proofErr == nil
			if valid != (test.postRunEdits == 0) {
				t.Errorf("post-run edits=%d: recovery authorized=%t error=%v", test.postRunEdits, valid, proofErr)
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
							statement := "UPDATE native_attempts SET sequence=sequence+1 WHERE organization_id=$1 AND id=$2"
							id := f.attempt
							if changed == "issue revision" {
								statement, id = "UPDATE issues SET revision=revision+1 WHERE organization_id=$1 AND native_id=$2", string(issue.WorkItemID)
							}
							if _, err := admin.ExecContext(ctx, statement, scope.organization, id); err != nil {
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
					statement := "UPDATE native_attempts SET sequence=sequence-1 WHERE organization_id=$1 AND id=$2"
					id := f.attempt
					if changed == "issue revision" {
						statement, id = "UPDATE issues SET revision=revision-1 WHERE organization_id=$1 AND native_id=$2", string(issue.WorkItemID)
					}
					if _, err := admin.ExecContext(t.Context(), statement, scope.organization, id); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, response, http.StatusConflict)
				})
			}
			objects.mu.Lock()
			readsBefore := objects.reads
			objects.mu.Unlock()
			probe.writerReads.Store(0)
			probe.writerEntries.Store(0)
			ctx := context.WithValue(t.Context(), payloadRecoveryProbeKey{}, probe)
			response = httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/workflow", f.worker, transition).WithContext(ctx))
			requireNativeStatus(t, response, http.StatusOK)
			if probe.writerEntries.Load() == 0 {
				t.Fatal("recovery writer history probe did not observe the real command")
			}
			if writerHistoryReads := probe.writerReads.Load(); writerHistoryReads != 0 {
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
			f.service.config.Content, f.service.config.RunDetails = nil, nil
			if storage != "unconfigured" {
				outage := fmt.Errorf("private-storage-endpoint: %w", artifact.ErrStorage)
				if storage == "untyped outage" {
					outage = errors.New("private-storage-endpoint")
				}
				f.service.config.RunDetails = outageRunDetails{err: outage}
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
			err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.Attempt(t.Context(), f.attempt)
				sequence = row.Sequence
				return err
			})
			if err != nil || sequence != 1 {
				t.Fatalf("failed storage write changed attempt: sequence=%d error=%v", sequence, err)
			}
		})
	}
}

type outageRunDetails struct{ err error }

func (o outageRunDetails) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return attachment.ArchiveReference{}, o.err
}

func (o outageRunDetails) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, o.err
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
			var bundle []byte
			if test.legacy && version.Source != nil {
				row, err := f.service.readChangeVersionRecord(t.Context(), f.project.OrganizationID, f.change.ID, version.ID)
				if err != nil {
					t.Fatal(err)
				}
				bundle, err = eventstore.NewContent(f.service.runDetailStore()).SourceBundle(t.Context(), f.project.OrganizationID, row, *version.Source)
				if err != nil {
					t.Fatal(err)
				}
			}
			change := f.change
			if test.legacy {
				change.ID, version.ID = newNativeID("change"), newNativeID("version")
				version.ChangeID, change.CurrentVersion, change.Revision, change.Landed = change.ID, version.ID, 1, nil
			}
			scope := analyticsTestScope(t, f.nativeFixture)
			receipt := tracker.NativeLandingReceipt{VersionID: version.ID, ChangeID: change.ID, HeadSHA: version.HeadSHA, BaseRef: "develop", MergeSHA: strings.Repeat("c", 40), Landed: true, ObservedAt: version.CreatedAt.Add(time.Minute), Gate: &gate.CommandResult{Command: "go test ./...", Output: strings.Repeat("gate evidence", 8192)}}
			sourceReceipt := receipt
			attempt, sourceRef := payloadSeedLandingAttempt(t, f, sourceReceipt)
			if test.different {
				receipt.MergeSHA = strings.Repeat("d", 40)
				receipt.Gate = new(*receipt.Gate)
				receipt.Gate.Output = "historical receipt"
			}
			content := eventstore.NewContent(f.service.runDetailStore())
			var row eventstore.LandingReceipt
			if test.legacy {
				fixture := newCoreNeutralFixture(t, scope.organization, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
				fixture.add("policy_revisions", map[string]any{"scope": string(scope.organization) + "/" + string(scope.project), "policy_id": version.PolicyID, "metadata_json": string(historyTestJSON(t, version.Policy))})
				fixture.add("change_requests", map[string]any{"id": change.ID, "organization_id": scope.organization, "project_id": scope.project, "work_item_id": f.issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
				for _, item := range change.LinkedIssues {
					fixture.add("change_issue_links", map[string]any{"change_id": change.ID, "organization_id": scope.organization, "project_id": scope.project, "work_item_id": item})
				}
				fixture.add("change_versions", map[string]any{"id": version.ID, "change_id": change.ID, "number": version.Number, "policy_revision_id": version.PolicyID, "record_json": string(historyTestJSON(t, version)), "repository": version.Repository})
				if version.Source != nil {
					fixture.add("change_sources", map[string]any{"version_id": version.ID, "bundle": bundle, "object_key": "", "object_sha256": "", "object_bytes": 0, "content_bytes": 0})
				}
				fixture.add("change_landing_receipts", map[string]any{"attempt_id": attempt.ID, "version_id": version.ID, "record_json": string(historyTestJSON(t, receipt))})
				objects := f.service.runDetailStore()
				source := CoreChangeImportSource{BundlePath: fixture.write(), OrganizationID: scope.organization, SourceObjects: objects, Objects: objects}
				var first eventstore.LandingReceipt
				for pass := range 2 {
					count, err := ImportCoreChanges(t.Context(), f.service.config.TenantRepository.(*postgres.Repository), source)
					if err != nil || count != 1 {
						t.Fatalf("historical landing import count=%d error=%v", count, err)
					}
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						row, err = r.LandingReceipt(t.Context(), attempt.ID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if pass != 0 && row != first {
						t.Fatal("historical landing replay changed canonical reference")
					}
					first = row
				}
			} else {
				var err error
				row, err = content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: scope.organization, AttemptID: attempt.ID, VersionID: version.ID, Repository: version.Repository, BaseRef: receipt.BaseRef}, receipt)
				if err != nil {
					t.Fatal(err)
				}
				coordinatorFixtureWrite(t, f.service, scope.organization, func(w tenantstore.Writer) error {
					var err error
					row, err = w.CreateLandingReceipt(t.Context(), row)
					return err
				})
			}
			before, err := content.LandingReceipt(t.Context(), row)
			if err != nil || !reflect.DeepEqual(before, receipt) {
				t.Fatalf("receipt after publication differs: %v", err)
			}
			canonical, _, err := attachment.PrepareArchive(string(scope.organization), 1, bytes.NewReader(historyTestJSON(t, receipt)))
			if err != nil {
				t.Fatal(err)
			}
			if row.OrganizationID != scope.organization || row.AttemptID != attempt.ID || row.VersionID != version.ID || row.MergeSHA != receipt.MergeSHA || eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes) != canonical {
				t.Fatal("landing receipt lost scoped canonical custody or typed merge SHA")
			}
			matching, _, err := attachment.PrepareArchive(string(scope.organization), 1, bytes.NewReader(historyTestJSON(t, sourceReceipt)))
			if err != nil {
				t.Fatal(err)
			}
			if (canonical.Key != matching.Key) != test.different {
				t.Fatal("landing receipt did not preserve or distinguish the canonical source receipt")
			}
			selected, err := f.service.readNativeAttemptTenant(t.Context(), scope, string(f.issue.WorkItemID), attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			sourceRaw, err := f.service.runDetailStore().ReadDetail(t.Context(), string(scope.organization), sourceRef)
			if err != nil {
				t.Fatal(err)
			}
			var sourceRun tracker.NativeRunData
			if json.Unmarshal(sourceRaw, &sourceRun) != nil || sourceRun.Runtime == nil || !reflect.DeepEqual(sourceRun.Runtime.Landing, &sourceReceipt) {
				t.Fatal("landing publication changed immutable source runtime receipt")
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				current, err := r.Attempt(t.Context(), attempt.ID)
				if err == nil && current.PayloadObjectKey != sourceRef.Key {
					return artifact.ErrIntegrity
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			repository := validationRepository(version.Repository)
			scheduled := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("a", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Check", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: receipt.MergeSHA, Conclusion: "failure"}
			body := issueorigin.Stamp("Scheduled check failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/" + repository + "/actions/runs/123", Fingerprint: scheduled.OccurrenceKey})
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "landing-validation"}, Body: body}), http.StatusOK)
			issue, err := coordinatorActionIssue(t.Context(), f.service, scope.organization, scope.project, f.issue.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			if test.invalid != "" {
				invalid, err := content.PreparePayload(t.Context(), scope.organization, []byte(test.invalid))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `UPDATE change_landing_receipts SET payload_object_key=$1,payload_sha256=$2,payload_bytes=$3,payload_content_bytes=$4 WHERE organization_id=$5 AND attempt_id=$6`, invalid.Key, invalid.SHA256, invalid.CompressedSize, invalid.UncompressedSize, scope.organization, attempt.ID); err != nil {
					t.Fatal(err)
				}
				row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = invalid.Key, invalid.SHA256, invalid.CompressedSize, invalid.UncompressedSize
			}
			after, err := content.LandingReceipt(t.Context(), row)
			if test.invalid != "" {
				if !errors.Is(err, artifact.ErrIntegrity) {
					t.Errorf("invalid archived landing receipt returned %v, want typed integrity error", err)
				}
				_, err = f.service.readValidationAuditRecord(t.Context(), scope, issue, &selected)
				if !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("validation accepted invalid archived landing receipt: %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(after, receipt) {
				t.Fatalf("receipt after conversion differs: %v", err)
			}
			audit, err := f.service.readValidationAuditRecord(t.Context(), scope, issue, &selected)
			if err != nil || audit == nil || len(audit.Occurrences) != 1 || len(audit.Occurrences[0].Comparisons) != 1 {
				t.Fatalf("validation candidates missing: audit=%+v error=%v", audit, err)
			}
			candidate := audit.Occurrences[0].Comparisons[0]
			if candidate.IntegratedHeadSHA != receipt.MergeSHA || candidate.IntegratedSource != "same_commit" || !candidate.ObservedAt.Equal(receipt.ObservedAt) || candidate.NativeAttemptID != attempt.ID {
				t.Fatalf("validation lost landing provenance: %+v", candidate)
			}
			local := *receipt.Gate
			local.Output, local.Command = "", "go test [redacted]"
			if !reflect.DeepEqual(candidate.Local, &local) {
				t.Fatalf("validation lost landing gate: %+v", candidate.Local)
			}
		})
	}
}

type analyticsPayloadReadKey struct{}

func payloadEvidenceFixture(t *testing.T, s *Service, organization tracker.OrganizationID, change, version, kind, source string, value any) eventstore.Evidence {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	row := eventstore.Evidence{OrganizationID: organization, ChangeID: change, Kind: kind}
	if version != "" {
		row.VersionID = &version
	}
	if source != "" {
		row.SourceKey = &source
	}
	row, err = eventstore.NewContent(s.runDetailStore()).PrepareEvidence(t.Context(), row, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		row, err = w.AppendEvidence(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

func payloadAssertAttemptReferences(t *testing.T, s *Service, organization tracker.OrganizationID, id string) {
	t.Helper()
	var row leasestore.Attempt
	if err := s.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.Attempt(t.Context(), id)
		if err != nil {
			return err
		}
		object, err := r.ArchiveObject(t.Context(), "archive_"+row.PayloadObjectKey[strings.LastIndex(row.PayloadObjectKey, "/")+1:])
		if err != nil {
			return err
		}
		if object.Reference.Key != row.PayloadObjectKey || object.Reference.SHA256 == "" {
			return artifact.ErrIntegrity
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if row.PayloadObjectKey == "" || row.CheckpointObjectKey == nil || *row.CheckpointObjectKey == "" {
		t.Fatal("attempt runtime or checkpoint payload lacks an object reference")
	}
}

func payloadAssertHistoricalAttemptReads(t *testing.T, data tracker.NativeRunData, artifacts []string) {
	t.Helper()
	repository, admin := coreImportPostgresWithAdmin(t, 3, nil)
	objects := &memoryRunDetails{}
	service := &Service{config: Config{RunDetails: objects}}
	for _, organization := range []tracker.OrganizationID{"org_payload_import_first", "org_payload_import_second"} {
		seedCoreExecutionPayloadTarget(t, repository, admin, organization)
		fixture := coreExecutionNeutralFixture(t, organization)
		expected := map[string]tracker.NativeRunData{}
		for index, suffix := range []string{"", "_second"} {
			run := data
			run.AttemptID, run.RunID = "attempt_execution_import"+suffix, "run_execution_import"+suffix
			run.LeaseID, run.FencingToken = tracker.LeaseID("lease_execution_import"+suffix), tracker.FencingToken(40+index)
			run.MachineID, run.PolicyID = tracker.MachineID("machine_execution_import"+suffix), "policy_example"
			raw, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := json.Marshal(nativeTestCheckpoint())
			if err != nil {
				t.Fatal(err)
			}
			retained, err := json.Marshal(artifacts)
			if err != nil {
				t.Fatal(err)
			}
			fixture.set("native_attempts", map[string]string{"id": run.AttemptID}, map[string]any{"data_json": string(raw), "checkpoint_json": string(checkpoint), "artifact_ids_json": string(retained), "sequence": run.Sequence, "status": "succeeded"})
			for sequence := int64(2); sequence <= run.Sequence; sequence++ {
				fixture.add("native_attempt_events", map[string]any{"attempt_id": run.AttemptID, "sequence": sequence, "request_hash": strings.Repeat("0", 64)})
			}
			run.ArtifactIDs = append([]string(nil), artifacts...)
			expected[run.AttemptID] = run
		}
		source := CoreExecutionImportSource{BundlePath: fixture.write(), OrganizationID: organization, SourceObjects: objects, Objects: objects}
		previous := map[string]tracker.NativeAttempt{}
		charged := map[string]attachment.ArchiveReference{}
		for pass := range 2 {
			count, err := ImportCoreExecution(t.Context(), repository, source)
			if err != nil || count != 14 {
				t.Fatalf("historical payload import count=%d error=%v", count, err)
			}
			for _, suffix := range []string{"", "_second"} {
				scope := nativeScope{organization: organization, project: "prj_execution_import"}
				item := "wi_execution_import" + suffix
				id := "attempt_execution_import" + suffix
				var snapshot tenantAttemptSnapshot
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					snapshot.attempt, err = r.Attempt(t.Context(), id)
					if err != nil {
						return err
					}
					snapshot.lease, err = r.Lease(t.Context(), snapshot.attempt.LeaseID)
					if err != nil {
						return err
					}
					r, err = readAttemptMetadataSnapshot(t.Context(), r, scope, item, []leasestore.Attempt{snapshot.attempt}, false)
					if err != nil {
						return err
					}
					snapshot.event.OrganizationID, snapshot.event.ProjectID, snapshot.event.WorkItemID = organization, scope.project, tracker.NativeWorkItemID(item)
					return readTenantAttemptPayload(t.Context(), r, &snapshot)
				}); err != nil {
					t.Fatal(err)
				}
				if snapshot.payload == nil || snapshot.checkpointPayload == nil || snapshot.artifactsPayload == nil {
					t.Fatal("historical payload import left inline or unowned attempt content")
				}
				for _, ref := range []attachment.ArchiveReference{*snapshot.payload, *snapshot.checkpointPayload, *snapshot.artifactsPayload} {
					if pass == 0 {
						charged[ref.Key] = ref
					} else if charged[ref.Key] != ref {
						t.Fatal("historical payload replay changed charged archive custody")
					}
				}
				got, err := service.hydrateNativeAttemptTenant(t.Context(), snapshot, time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.NativeRunData, expected[id]) || !reflect.DeepEqual(got.Checkpoint, nativeTestCheckpoint()) {
					t.Fatal("historical attempt import changed full payload or retained checkpoint artifacts")
				}
				if pass != 0 && !reflect.DeepEqual(got, previous[id]) {
					t.Fatal("backfill changed full attempt reads")
				}
				previous[id] = got
			}
			if pass == 0 {
				payloadAssertImportedCounters(t, repository, organization, nil)
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					for _, ref := range charged {
						result, err := w.ChargeArchiveObject(t.Context(), "archive_"+ref.SHA256, 1<<30)
						if err != nil {
							return err
						}
						if !result.Allowed || !result.Applied {
							t.Fatal("historical payload archive did not receive its first scoped debit")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			payloadAssertImportedCounters(t, repository, organization, charged)
		}
	}
}

func payloadAssertHistoricalVersion(t *testing.T, f changeFixture, version tracker.ChangeVersion, bundle []byte) {
	t.Helper()
	legacy := version
	legacy.ID, legacy.ChangeID, legacy.Number = newNativeID("version"), newNativeID("change"), version.Number+1000
	change := f.change
	change.ID, change.CurrentVersion, change.Revision, change.Landed = legacy.ChangeID, legacy.ID, 1, nil
	fixture := newCoreNeutralFixture(t, f.project.OrganizationID, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.add("policy_revisions", map[string]any{"scope": string(f.project.OrganizationID) + "/" + string(f.project.ID), "policy_id": legacy.PolicyID, "metadata_json": string(historyTestJSON(t, legacy.Policy))})
	fixture.add("change_requests", map[string]any{"id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": f.issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
	for _, item := range change.LinkedIssues {
		fixture.add("change_issue_links", map[string]any{"change_id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item})
	}
	fixture.add("change_versions", map[string]any{"id": legacy.ID, "change_id": change.ID, "number": legacy.Number, "record_json": string(historyTestJSON(t, legacy)), "policy_revision_id": legacy.PolicyID})
	fixture.add("change_sources", map[string]any{"version_id": legacy.ID, "bundle": bundle, "object_key": "", "object_sha256": "", "object_bytes": 0, "content_bytes": 0})
	objects := f.service.runDetailStore()
	source := CoreChangeImportSource{BundlePath: fixture.write(), OrganizationID: f.project.OrganizationID, SourceObjects: objects, Objects: objects}
	repository := f.service.config.TenantRepository.(*postgres.Repository)
	var before tracker.ChangeVersion
	for pass := range 2 {
		count, err := ImportCoreChanges(t.Context(), repository, source)
		if err != nil || count != 1 {
			t.Fatalf("historical version import count=%d error=%v", count, err)
		}
		row, err := f.service.readChangeVersionRecord(t.Context(), f.project.OrganizationID, change.ID, legacy.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.PayloadObjectKey == "" || row.SourceObjectKey == "" {
			t.Fatal("historical version/source content remained inline")
		}
		content := eventstore.NewContent(objects)
		after, err := content.Version(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, legacy) {
			t.Fatal("legacy version backfill changed reads")
		}
		if pass != 0 && !reflect.DeepEqual(after, before) {
			t.Fatal("legacy version replay changed reads")
		}
		got, err := content.SourceBundle(t.Context(), f.project.OrganizationID, row, *after.Source)
		if err != nil || !bytes.Equal(got, bundle) {
			t.Fatalf("legacy source bytes changed: %v", err)
		}
		before = after
	}
}

func payloadAssertHistoricalDiff(t *testing.T, patch string) {
	t.Helper()
	repository, admin := coreImportPostgresWithAdmin(t, 3, nil)
	objects := &memoryRunDetails{}
	service := &Service{config: Config{TenantRepository: repository, RunDetails: objects}}
	for _, organization := range []tracker.OrganizationID{"org_payload_diff_first", "org_payload_diff_second"} {
		seedCoreExecutionPayloadTarget(t, repository, admin, organization)
		fixture := coreExecutionNeutralFixture(t, organization)
		for index, suffix := range []string{"", "_second"} {
			diffID := "diff_payload_import" + suffix
			fixture.add("attempt_diffs", map[string]any{"id": diffID, "organization_id": organization, "project_id": "prj_execution_import", "work_item_id": "wi_execution_import" + suffix, "attempt_id": "attempt_execution_import" + suffix, "source": "attempt", "source_id": "", "seq": 1, "base_sha": strings.Repeat("a", 40), "head_sha": strings.Repeat("b", 40), "producer_kind": "attempt", "producer_id": "attempt_execution_import" + suffix, "producer_runner_id": "runner_execution_import" + suffix, "producer_lease_id": "lease_execution_import" + suffix, "producer_fencing_token": 40 + index, "file_count": 1, "patch_bytes": len(patch), "posted_bytes": len(patch), "created_at": "2026-10-02T00:00:00Z"})
			fixture.add("attempt_diff_files", map[string]any{"diff_id": diffID, "position": 0, "path": "example.go", "old_path": "", "status": "modified", "additions": 0, "deletions": 0, "binary": false, "patch": patch, "truncated": false, "denied": false, "patch_expired": false})
		}
		source := CoreExecutionImportSource{BundlePath: fixture.write(), OrganizationID: organization, SourceObjects: objects, Objects: objects}
		previous := map[string]tracker.AttemptDiff{}
		for pass := range 2 {
			count, err := ImportCoreExecution(t.Context(), repository, source)
			if err != nil || count != 16 {
				t.Fatalf("historical diff import count=%d error=%v", count, err)
			}
			for _, suffix := range []string{"", "_second"} {
				id := "attempt_execution_import" + suffix
				got, err := service.readAttemptDiffTenant(t.Context(), nativeScope{organization: organization, project: "prj_execution_import"}, "wi_execution_import"+suffix, id, "attempt", 1)
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Files) != 1 || got.Files[0].Patch != patch || got.PatchBytes != int64(len(patch)) {
					t.Fatal("diff backfill changed body bytes")
				}
				if pass != 0 && !reflect.DeepEqual(got, previous[id]) {
					t.Fatal("diff backfill changed reads")
				}
				previous[id] = got
			}
		}
	}
}

func payloadSeedRecoveryEdits(t *testing.T, f nativeFixture, issue tracker.NativeIssue, count int) tracker.NativeIssue {
	t.Helper()
	content := eventstore.NewContent(f.service.runDetailStore())
	events := make([]eventstore.Event, 0, count+1)
	for index := range count + 1 {
		actor := tracker.Actor{Kind: "human", PrincipalID: "example-editor"}
		kind := "issue.edited"
		data := tracker.CollaborationData{Revision: issue.Revision + tracker.Revision(index) + 1, Fields: []string{"body"}}
		if index == count {
			actor = tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}
			kind = "workflow.transitioned"
			data = tracker.CollaborationData{Revision: data.Revision, FromState: "Todo", ToState: "Blocked", Reason: "worker_progress"}
		}
		event := tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: kind, SchemaVersion: 1, RecordedAt: f.service.config.now(), Actor: actor, Data: data}
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, raw)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, prepared)
	}
	issue.Revision += tracker.Revision(count) + 1
	issue.UpdatedAt = f.service.config.now()
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	coordinatorFixtureWrite(t, f.service, f.project.OrganizationID, func(w tenantstore.Writer) error {
		for _, event := range events {
			row, err := w.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
			if err != nil {
				return err
			}
			if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
				return err
			}
			if _, err := w.AppendEvent(t.Context(), event); err != nil {
				return err
			}
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	})
	return issue
}

type payloadRecoveryProbeKey struct{}

type payloadRecoveryRepository struct {
	*postgres.Repository
	rows          atomic.Int64
	writerReads   atomic.Int64
	writerEntries atomic.Int64
}

func (p *payloadRecoveryRepository) reader(ctx context.Context, r tenantstore.Reader) tenantstore.Reader {
	if ctx.Value(payloadRecoveryProbeKey{}) != p {
		return r
	}
	return payloadRecoveryReader{Reader: r, WorkflowTransitionReader: r.(eventstore.WorkflowTransitionReader), AttemptMetadataReader: r.(tenantstore.AttemptMetadataReader), probe: p}
}

func (p *payloadRecoveryRepository) writer(ctx context.Context, w tenantstore.Writer) tenantstore.Writer {
	if ctx.Value(payloadRecoveryProbeKey{}) != p {
		return w
	}
	p.writerEntries.Add(1)
	return payloadRecoveryWriter{Writer: w, WorkflowTransitionReader: w.(eventstore.WorkflowTransitionReader), RecordedRecoveryWriter: w.(leasestore.RecordedRecoveryWriter), FencedLeaseLocker: w.(leasestore.FencedLeaseLocker), UnreleasedIssueLeaseLocker: w.(leasestore.UnreleasedIssueLeaseLocker), RetentionWriter: w.(receiptstore.RetentionWriter), GitHubOutboxWriter: w.(projectstore.GitHubOutboxWriter), probe: p}
}

func (p *payloadRecoveryRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return p.Repository.ReadTenant(ctx, org, workload, func(r tenantstore.Reader) error { return read(p.reader(ctx, r)) })
}

func (p *payloadRecoveryRepository) ReadTenantSnapshot(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return p.Repository.ReadTenantSnapshot(ctx, org, workload, func(r tenantstore.Reader) error { return read(p.reader(ctx, r)) })
}

func (p *payloadRecoveryRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return p.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error { return write(p.writer(ctx, w)) })
}

func (p *payloadRecoveryRepository) WriteRunnerCommand(ctx context.Context, org tracker.OrganizationID, credential, runner string, write func(context.Context, tenantstore.Writer) error) error {
	return p.Repository.WriteRunnerCommand(ctx, org, credential, runner, func(ctx context.Context, w tenantstore.Writer) error { return write(ctx, p.writer(ctx, w)) })
}

type payloadRecoveryReader struct {
	tenantstore.Reader
	eventstore.WorkflowTransitionReader
	tenantstore.AttemptMetadataReader
	probe *payloadRecoveryRepository
}

func (r payloadRecoveryReader) AttemptMetadata(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, attempts []leasestore.Attempt, evidence bool) (tenantstore.AttemptMetadata, error) {
	metadata, err := r.AttemptMetadataReader.AttemptMetadata(ctx, project, item, attempts, evidence)
	r.probe.rows.Add(int64(len(metadata.Events)))
	return metadata, err
}

func (r payloadRecoveryReader) LatestRunEvent(ctx context.Context, project tracker.ProjectID, id string) (eventstore.Event, error) {
	row, err := r.Reader.LatestRunEvent(ctx, project, id)
	if err == nil {
		r.probe.rows.Add(1)
	}
	return row, err
}

func (r payloadRecoveryReader) LatestWorkflowTransition(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.Event, error) {
	row, err := r.WorkflowTransitionReader.LatestWorkflowTransition(ctx, project, item)
	if err == nil {
		r.probe.rows.Add(1)
	}
	return row, err
}

type payloadRecoveryWriter struct {
	tenantstore.Writer
	eventstore.WorkflowTransitionReader
	leasestore.RecordedRecoveryWriter
	leasestore.FencedLeaseLocker
	leasestore.UnreleasedIssueLeaseLocker
	receiptstore.RetentionWriter
	projectstore.GitHubOutboxWriter
	probe *payloadRecoveryRepository
}

func (w payloadRecoveryWriter) LatestRunEvent(ctx context.Context, project tracker.ProjectID, id string) (eventstore.Event, error) {
	w.probe.writerReads.Add(1)
	return w.Writer.LatestRunEvent(ctx, project, id)
}

func (w payloadRecoveryWriter) LatestWorkflowTransition(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.Event, error) {
	w.probe.writerReads.Add(1)
	return w.WorkflowTransitionReader.LatestWorkflowTransition(ctx, project, item)
}

func payloadSeedLandingAttempt(t *testing.T, f changeFixture, receipt tracker.NativeLandingReceipt) (leasestore.Attempt, attachment.ArchiveReference) {
	t.Helper()
	leaseID := newNativeID("lease")
	lease := coordinatorActionLease(t, f.service, f.project.OrganizationID, f.issue.WorkItemID, leaseID)
	data := tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: tracker.LeaseID(leaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Landing: &receipt}}
	content := eventstore.NewContent(f.service.runDetailStore())
	ref, err := content.PreparePayload(t.Context(), f.project.OrganizationID, historyTestJSON(t, data))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: f.issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, RecordedAt: receipt.ObservedAt, Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	var attempt leasestore.Attempt
	coordinatorFixtureWrite(t, f.service, f.project.OrganizationID, func(w tenantstore.Writer) error {
		var err error
		attempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, RunID: data.RunID, Sequence: 1, Status: "succeeded", StartedAt: receipt.ObservedAt, PayloadObjectKey: ref.Key, PayloadReference: ref})
		if err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: leaseID, FencingToken: lease.FencingToken}, "released")
		return err
	})
	return attempt, ref
}

func payloadAssertImportedCounters(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, charged map[string]attachment.ArchiveReference) {
	t.Helper()
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		population, ok := r.(billingstore.PopulationReader)
		if !ok {
			t.Fatal("historical payload import has no typed billing population owner")
		}
		for _, metric := range []struct {
			name string
			want int64
		}{{"events_total", 0}, {"history_records", 8}} {
			got, known, err := population.BillingPopulation(t.Context(), metric.name, time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), time.Minute)
			if err != nil {
				return err
			}
			if !known || got != metric.want {
				t.Fatalf("historical payload %s counter=%d known=%t want=%d", metric.name, got, known, metric.want)
			}
		}
		var expected int64
		for _, ref := range charged {
			row, err := r.ArchiveObject(t.Context(), "archive_"+ref.SHA256)
			if err != nil {
				return err
			}
			if row.Reference != ref || row.CollaborationBytes != ref.CompressedSize {
				t.Fatal("historical payload lost its exact archive debit")
			}
			expected += ref.CompressedSize
		}
		got, err := r.CollaborationBytes(t.Context())
		if err != nil {
			return err
		}
		if got != expected {
			t.Fatalf("historical payload collaboration byte counter=%d scoped debit recount=%d", got, expected)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
