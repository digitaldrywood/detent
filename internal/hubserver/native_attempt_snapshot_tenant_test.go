package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptSnapshotContextKey struct{}

type attemptSnapshotRepository struct {
	tenantstore.Repository
	tenantstore.SnapshotRepository
	beforeLatest func() error
	archiveErr   error
	active       int
}

func (r *attemptSnapshotRepository) wrap(ctx context.Context, read func(tenantstore.Reader) error) func(tenantstore.Reader) error {
	if ctx.Value(attemptSnapshotContextKey{}) == nil {
		return read
	}
	return func(reader tenantstore.Reader) error {
		r.active++
		defer func() { r.active-- }()
		return read(attemptSnapshotReader{Reader: reader, owner: r, CheckpointAttemptReader: reader.(leasestore.CheckpointAttemptReader)})
	}
}

func (r *attemptSnapshotRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, r.wrap(ctx, read))
}

func (r *attemptSnapshotRepository) ReadTenantSnapshot(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.SnapshotRepository.ReadTenantSnapshot(ctx, org, workload, r.wrap(ctx, read))
}

type attemptSnapshotReader struct {
	tenantstore.Reader
	leasestore.CheckpointAttemptReader
	owner *attemptSnapshotRepository
}

func (r attemptSnapshotReader) LatestRunEvent(ctx context.Context, project tracker.ProjectID, attempt string) (eventstore.Event, error) {
	if hook := r.owner.beforeLatest; hook != nil {
		r.owner.beforeLatest = nil
		if err := hook(); err != nil {
			return eventstore.Event{}, err
		}
	}
	return r.Reader.LatestRunEvent(ctx, project, attempt)
}

func TestNativeAttemptSnapshotConcurrentAppend(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	serving, _ := repository.WorkloadCapacities()
	if serving < 2 {
		t.Fatal("fixture needs separate reader and writer serving slots")
	}
	participant := &nativeRunEventParticipantFixture{}
	keys := secretTestKeys(t, "1", "1")
	details := &verifiedAttemptDetails{}
	service := &Service{
		echo: echo.New(), database: &database{now: time.Now, billingRepository: repository, billingAuditStore: details}, secretRewrapReady: make(chan struct{}),
		notifications: newNotificationBroker(), boardRefresh: make(chan struct{}, 1),
		config: Config{
			Logger: discardLogger(), now: time.Now, generateToken: apikey.GenerateToken,
			TenantRepository: repository, AccessRepository: repository, RunDetails: details,
			NativeRunEvents: participant, SecretKeys: keys,
		},
	}
	service.ready.Store(true)
	service.registerNativeRoutes(service.echo)
	wrapped := &attemptSnapshotRepository{Repository: repository, SnapshotRepository: repository}
	service.config.TenantRepository = wrapped
	var otherAttempt string
	for _, organizationName := range []string{"Example organization one", "Example organization two"} {
		t.Run(organizationName, func(t *testing.T) {
			organization := nativeOrganization{ID: tracker.OrganizationID(newNativeID("org")), Name: organizationName, Slug: strings.ReplaceAll(strings.ToLower(organizationName), " ", "-")}
			now := service.config.now().UTC()
			cursorKey := bytes.Repeat([]byte{1}, 32)
			cursorEnvelope, err := keys.Seal(cursorKey, accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				t.Fatal(err)
			}
			seedPostgresHubAdmin(t, repository, service, organization)
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: cursorEnvelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursorEnvelope.Version, RecordedAt: now}); err != nil {
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
			projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{
				"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates(),
			})
			requireNativeStatus(t, projectResponse, http.StatusOK)
			var project tracker.NativeProject
			decodeHubResponse(t, projectResponse, &project)
			itemPath := projectPath + "/" + string(project.ID) + "/work-items"
			itemResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
				Mutation: tracker.Mutation{IdempotencyKey: "item"}, Title: "Example issue", Body: workItemRecordBody,
			})
			requireNativeStatus(t, itemResponse, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, itemResponse, &issue)
			var lease leasestore.Lease
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				workItem, err := w.WorkItem(t.Context(), project.ID, issue.WorkItemID)
				if err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: "machine", Hostname: "example", Capacity: 2, Version: "example"}); err != nil {
					return err
				}
				lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: "machine", SessionID: "session", LeaseID: "lease", TTL: time.Hour})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
			start := tracker.NativeRunEvent{
				Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
				Data: tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: tracker.LeaseID(lease.LeaseID),
					FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: "policy_" + strings.Repeat("a", 64), Sequence: 1,
					Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "example-model"}},
			}

			if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), start); err != nil {
				t.Fatal(err)
			}
			var issueID int64
			if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), project.ID, issue.WorkItemID)
				issueID = row.ID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			t.Run("canonical_payload", func(t *testing.T) {
				var original leasestore.Attempt
				if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					original, err = r.Attempt(t.Context(), start.Data.AttemptID)
					if err != nil {
						return err
					}
					raw := details.data[original.PayloadObjectKey]
					ref, _, err := attachment.PrepareArchive(string(organization.ID), 1, bytes.NewReader(raw))
					if err != nil {
						return err
					}
					_, err = r.(interface {
						ArchiveObjectByReference(context.Context, attachment.ArchiveReference) (artifactstore.ArchiveObject, error)
					}).ArchiveObjectByReference(t.Context(), ref)
					return err
				}); err != nil {
					t.Errorf("live writer canonical metadata: %v", err)
				}
				defer func() {
					if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET sequence=1,payload_object_key=$1 WHERE organization_id=$2 AND id=$3", original.PayloadObjectKey, organization.ID, original.ID); err != nil {
						t.Error(err)
					}
				}()
				for _, test := range []struct {
					name     string
					change   func(*tracker.NativeRunData)
					register bool
					sequence int64
					wantErr  bool
				}{
					{"canonical outruns event", nil, true, 2, false},
					{"absent matching event", nil, false, 1, false},
					{"absent stale event", nil, false, 2, true},
					{"tampered content", nil, true, 2, true},
					{"wrong length", nil, true, 2, true},
					{"foreign key", nil, false, 1, true},
					{"malformed key", nil, false, 1, true},
					{"attempt identity", func(r *tracker.NativeRunData) { r.AttemptID += "wrong" }, true, 2, true},
					{"run identity", func(r *tracker.NativeRunData) { r.RunID += "wrong" }, true, 2, true},
					{"lease identity", func(r *tracker.NativeRunData) { r.LeaseID += "wrong" }, true, 2, true},
					{"fence identity", func(r *tracker.NativeRunData) { r.FencingToken++ }, true, 2, true},
					{"sequence identity", func(r *tracker.NativeRunData) { r.Sequence++ }, true, 2, true},
				} {
					t.Run(test.name, func(t *testing.T) {
						run := start.Data
						run.Sequence = 2
						if test.change != nil {
							test.change(&run)
						}
						raw, err := json.Marshal(run)
						if err != nil {
							t.Fatal(err)
						}
						ref, err := details.StoreDetail(t.Context(), string(organization.ID), raw)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := admin.ExecContext(t.Context(), "DELETE FROM archive_objects WHERE organization_id=$1 AND object_key=$2", organization.ID, ref.Key); err != nil {
							t.Fatal(err)
						}
						if test.register {
							if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
								_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization.ID, ID: newNativeID("archive"), Reference: ref, CreatedAt: now})
								return err
							}); err != nil {
								t.Fatal(err)
							}
						}
						if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET sequence=$1,payload_object_key=$2 WHERE organization_id=$3 AND id=$4", test.sequence, ref.Key, organization.ID, original.ID); err != nil {
							t.Fatal(err)
						}
						reads := details.reads
						switch test.name {
						case "tampered content":
							details.data[ref.Key] = bytes.ReplaceAll(raw, []byte("example-model"), []byte("changed-model"))
							defer func() { details.data[ref.Key] = raw }()
						case "wrong length":
							if _, err := admin.ExecContext(t.Context(), "UPDATE archive_objects SET uncompressed_size=uncompressed_size+1 WHERE organization_id=$1 AND object_key=$2", organization.ID, ref.Key); err != nil {
								t.Fatal(err)
							}
						case "foreign key", "malformed key":
							key := "malformed"
							if test.name == "foreign key" {
								key, err = attachment.ArchiveKey("org_foreign", ref.SHA256)
								if err != nil {
									t.Fatal(err)
								}
							}
							if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET payload_object_key=$1 WHERE organization_id=$2 AND id=$3", key, organization.ID, original.ID); err != nil {
								t.Fatal(err)
							}
						}
						detail, detailErr := service.readNativeAttemptTenant(t.Context(), scope, string(issue.WorkItemID), original.ID)
						page, pageErr := service.readNativeAttemptPageTenant(t.Context(), scope, string(issue.WorkItemID), url.Values{}, json.Marshal)
						if (test.name == "foreign key" || test.name == "malformed key") && details.reads != reads {
							t.Fatal("invalid key accessed object storage")
						}
						if test.wantErr {
							if !errors.Is(detailErr, artifact.ErrIntegrity) || !errors.Is(pageErr, artifact.ErrIntegrity) || len(page.Items) != 0 {
								t.Fatalf("invalid canonical read: detail=%v page=%v", detailErr, pageErr)
							}
						} else {
							if detailErr != nil || pageErr != nil || len(page.Items) != 1 {
								t.Fatalf("canonical read: detail=%v page=%v", detailErr, pageErr)
							}
							for _, got := range []tracker.NativeAttempt{detail, page.Items[0]} {
								if got.Sequence != test.sequence || got.AttemptID != original.ID || got.RunID != original.RunID || got.LeaseID != tracker.LeaseID(lease.LeaseID) || got.FencingToken != tracker.FencingToken(lease.FencingToken) {
									t.Fatalf("canonical identity = %+v", got.NativeRunData)
								}
							}
						}
					})
				}
			})
			t.Run("second_item_and_writer_replay", func(t *testing.T) {
				response := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "second-item"}, Title: "Second example issue", Body: workItemRecordBody})
				requireNativeStatus(t, response, http.StatusOK)
				var second tracker.NativeIssue
				decodeHubResponse(t, response, &second)
				var secondLease leasestore.Lease
				if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), project.ID, second.WorkItemID)
					if err != nil {
						return err
					}
					secondLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine", SessionID: "second-session", LeaseID: "second-lease", TTL: time.Hour})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				event := start
				event.IdempotencyKey = "second-start"
				event.Data.AttemptID, event.Data.RunID = newNativeID("attempt"), newNativeID("run")
				event.Data.LeaseID, event.Data.FencingToken = tracker.LeaseID(secondLease.LeaseID), tracker.FencingToken(secondLease.FencingToken)
				for range 2 {
					if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(second.WorkItemID), event); err != nil {
						t.Fatal(err)
					}
				}
				got, err := service.readNativeAttemptTenant(t.Context(), scope, string(second.WorkItemID), event.Data.AttemptID)
				if err != nil || got.Sequence != 1 {
					t.Fatalf("second attempt: %+v %v", got, err)
				}
				if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
					attempt, err := r.Attempt(t.Context(), event.Data.AttemptID)
					if err != nil {
						return err
					}
					_, err = r.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), attempt.PayloadObjectKey)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				run := got.NativeRunData
				run.Sequence = 2
				raw, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := details.StoreDetail(t.Context(), string(organization.ID), raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := validateNativeDetailReference(t.Context(), details, organization.ID, attachment.ArchiveReference{}, raw); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("run-event accepted absent reference: %v", err)
				}
				write := leasestore.AttemptEvent{ID: run.AttemptID, RunID: run.RunID, Fence: leasestore.Fence{LeaseID: secondLease.LeaseID, FencingToken: secondLease.FencingToken}, Sequence: 2, Status: "running", DispatchGeneration: 7, PayloadObjectKey: ref.Key, PayloadReference: ref}
				persist := func(input leasestore.AttemptEvent) (bool, error) {
					var replay bool
					err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
						_, duplicate, err := w.RecordAttemptEvent(t.Context(), input)
						replay = duplicate
						return err
					})
					return replay, err
				}
				for _, change := range []func(*leasestore.AttemptEvent){
					func(e *leasestore.AttemptEvent) { e.PayloadObjectKey = "malformed" },
					func(e *leasestore.AttemptEvent) { e.PayloadReference.CompressedSize++ },
				} {
					if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization.ID, ID: newNativeID("archive"), Reference: ref, CreatedAt: now})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					invalid := write
					change(&invalid)
					if _, err := persist(invalid); err == nil {
						t.Fatal("invalid write accepted")
					}
					var sequence, receipts, counters int64
					if err := admin.QueryRowContext(t.Context(), "SELECT sequence,(SELECT count(*) FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2),(SELECT count(*) FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2) FROM native_attempts WHERE organization_id=$1 AND id=$2", organization.ID, run.AttemptID).Scan(&sequence, &receipts, &counters); err != nil {
						t.Fatal(err)
					}
					if sequence != 1 || receipts != 1 || counters != 0 {
						t.Fatalf("rejected write changed state: %d %d %d", sequence, receipts, counters)
					}
				}
				for _, wantReplay := range []bool{false, true} {
					if wantReplay {
						if _, err := admin.ExecContext(t.Context(), "DELETE FROM archive_objects WHERE organization_id=$1 AND object_key=$2", organization.ID, ref.Key); err != nil {
							t.Fatal(err)
						}
					}
					duplicate, err := persist(write)
					if err != nil || duplicate != wantReplay {
						t.Fatalf("write replay=%v want=%v: %v", duplicate, wantReplay, err)
					}
					if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
						object, err := r.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), ref.Key)
						if err == nil && object.Reference != ref {
							return errors.New("replayed metadata differs")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}

				write.Sequence, write.Status = 3, "cancelled"
				write.PayloadReference = attachment.ArchiveReference{}
				for _, wantReplay := range []bool{false, true} {
					duplicate, err := persist(write)
					if err != nil || duplicate != wantReplay {
						t.Fatalf("cancellation replay=%v want=%v: %v", duplicate, wantReplay, err)
					}
				}
				if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
					attempt, err := r.Attempt(t.Context(), write.ID)
					if err == nil && (attempt.Status != "cancelled" || attempt.Sequence != 3) {
						return fmt.Errorf("cancellation not persisted: %+v", attempt)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}

			})
			t.Run("metadata_unavailable", func(t *testing.T) {
				wrapped.archiveErr = errors.New("metadata unavailable")
				defer func() { wrapped.archiveErr = nil }()
				ctx := context.WithValue(t.Context(), attemptSnapshotContextKey{}, true)
				reads := details.reads
				_, err := service.readNativeAttemptTenant(ctx, scope, string(issue.WorkItemID), start.Data.AttemptID)
				if !errors.Is(err, wrapped.archiveErr) || details.reads != reads {
					t.Fatalf("metadata failure fell back: %v", err)
				}
			})
			readContext := context.WithValue(t.Context(), attemptSnapshotContextKey{}, true)
			readers := []struct {
				name string
				read func() (tracker.NativeAttempt, error)
			}{
				{"single", func() (tracker.NativeAttempt, error) {
					return service.readNativeAttemptTenant(readContext, scope, string(issue.WorkItemID), start.Data.AttemptID)
				}},
				{"page", func() (tracker.NativeAttempt, error) {
					page, err := service.readNativeAttemptPageTenant(readContext, scope, string(issue.WorkItemID), url.Values{}, json.Marshal)
					if err != nil {
						return tracker.NativeAttempt{}, err
					}
					if len(page.Items) != 1 {
						return tracker.NativeAttempt{}, fmt.Errorf("attempt page count = %d", len(page.Items))
					}
					return page.Items[0], nil
				}},
				{"initial_recovery", func() (tracker.NativeAttempt, error) {
					recovery, err := service.readNativeRecoveryTenant(readContext, scope, string(issue.WorkItemID))
					if err != nil {
						return tracker.NativeAttempt{}, err
					}
					if len(recovery.Attempts) != 1 {
						return tracker.NativeAttempt{}, fmt.Errorf("recovery attempt count = %d", len(recovery.Attempts))
					}
					return recovery.Attempts[0], nil
				}},
				{"selected_recovery", func() (tracker.NativeAttempt, error) {
					snapshot, err := service.readNativeRecoveryAttemptSnapshot(readContext, scope, string(issue.WorkItemID), issueID, start.Data.AttemptID)
					if err != nil {
						return tracker.NativeAttempt{}, err
					}
					return service.hydrateNativeAttemptTenant(readContext, snapshot, service.config.now())
				}},
			}
			sequence := int64(1)
			for _, reader := range readers {
				t.Run(reader.name, func(t *testing.T) {
					previous := sequence
					sequence++
					observation := start
					observation.IdempotencyKey = fmt.Sprintf("observation-%d", sequence)
					observation.Type = "run.observed"
					observation.Data.Sequence = sequence
					observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: service.config.now().UTC()}
					appended := false
					wrapped.beforeLatest = func() error {
						if wrapped.active != 1 {
							return fmt.Errorf("reader transaction count = %d", wrapped.active)
						}
						_, err := service.appendNativeRunEventTenant(t.Context(), scope, string(issue.WorkItemID), observation)
						appended = err == nil
						return err
					}
					defer func() { wrapped.beforeLatest = nil; details.beforeRead = nil }()
					details.beforeRead = func(ctx context.Context) error {
						if ctx.Value(attemptSnapshotContextKey{}) != nil && wrapped.active != 0 {
							return errors.New("object read inside serving transaction")
						}
						return nil
					}
					value, err := reader.read()
					if !appended {
						t.Fatal("canonical append did not commit inside reader transaction")
					}
					if err != nil {
						t.Fatalf("concurrent append read: %v", err)
					}
					if value.Sequence != previous || value.AttemptID != start.Data.AttemptID || value.RunID != start.Data.RunID || value.LeaseID != start.Data.LeaseID || value.FencingToken != start.Data.FencingToken {
						t.Fatalf("inconsistent snapshot: %+v", value.NativeRunData)
					}
					current, err := service.readNativeAttemptTenant(readContext, scope, string(issue.WorkItemID), start.Data.AttemptID)
					if err != nil || current.Sequence != sequence {
						t.Fatalf("committed append sequence = %d, want %d: %v", current.Sequence, sequence, err)
					}
				})
			}

			if otherAttempt != "" {
				_, err := service.readNativeAttemptTenant(readContext, scope, string(issue.WorkItemID), otherAttempt)
				if !errors.Is(err, corestore.ErrNotFound) && !isNativeNotFound(err) {
					t.Fatalf("cross-organization attempt read = %v", err)
				}
				_, err = service.readNativeRecoveryAttemptSnapshot(readContext, scope, string(issue.WorkItemID), issueID, otherAttempt)
				if !isNativeNotFound(err) {
					t.Fatalf("cross-organization selected recovery = %v", err)
				}
			}
			otherAttempt = start.Data.AttemptID
			if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET sequence = sequence + 1 WHERE organization_id = $1 AND id = $2", organization.ID, start.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			for _, reader := range readers {
				t.Run("stable_mismatch_"+reader.name, func(t *testing.T) {
					_, err := reader.read()
					if !errors.Is(err, artifact.ErrIntegrity) || !strings.Contains(err.Error(), "hydrate attempt identity") {
						t.Fatalf("stable identity mismatch = %v", err)
					}
				})
			}
		})
	}
}

func (r attemptSnapshotReader) ArchiveObjectByKey(ctx context.Context, key string) (artifactstore.ArchiveObject, error) {
	if r.owner.archiveErr != nil {
		return artifactstore.ArchiveObject{}, r.owner.archiveErr
	}
	return r.Reader.(attemptArchiveReader).ArchiveObjectByKey(ctx, key)
}

type verifiedAttemptDetails struct{ memoryRunDetails }
type attemptArchiveStorage struct {
	attachment.Storage
	data []byte
}

func (s attemptArchiveStorage) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
func (s *verifiedAttemptDetails) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	raw, err := s.memoryRunDetails.ReadDetail(ctx, org, ref)
	if err != nil {
		return nil, err
	}
	_, compressed, err := attachment.PrepareArchive(org, 1, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return attachment.ReadArchive(ctx, attemptArchiveStorage{data: compressed}, org, ref)
}
