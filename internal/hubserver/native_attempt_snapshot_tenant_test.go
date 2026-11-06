package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptSnapshotContextKey struct{}

type attemptAttachmentReader struct {
	tenantstore.Reader
	row attachmentstore.Record
}

func (r attemptAttachmentReader) Attachment(context.Context, tracker.ProjectID, string) (attachmentstore.Record, error) {
	return r.row, nil
}

func TestNativeAttemptAttachmentPayloadIntegrity(t *testing.T) {
	t.Parallel()
	ref, _, err := attachment.PrepareArchive("org_example", 1, strings.NewReader(`{"sequence":43}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*attachmentstore.Record)
	}{
		{"valid", nil},
		{"organization", func(r *attachmentstore.Record) { r.OrganizationID = "org_other" }},
		{"project", func(r *attachmentstore.Record) { r.Metadata.ProjectID = "prj_other" }},
		{"attachment", func(r *attachmentstore.Record) { r.Metadata.ID = "att_other" }},
		{"key", func(r *attachmentstore.Record) { r.ObjectKey += "wrong" }},
		{"digest", func(r *attachmentstore.Record) { r.Metadata.SHA256 = strings.Repeat("f", 64) }},
		{"size", func(r *attachmentstore.Record) { r.Metadata.Size = 0 }},
		{"content size", func(r *attachmentstore.Record) { r.UncompressedSize = 0 }},
		{"codec", func(r *attachmentstore.Record) { r.Codec = "plain" }},
		{"schema", func(r *attachmentstore.Record) { r.SchemaVersion = 2 }},
		{"kind", func(r *attachmentstore.Record) { r.Kind = "attachment" }},
		{"retention", func(r *attachmentstore.Record) { r.Retention = "temporary" }},
		{"binding", func(r *attachmentstore.Record) { r.StorageBindingID = "binding_example" }},
		{"customer owned", func(r *attachmentstore.Record) { r.CustomerOwned = true }},
		{"deleted", func(r *attachmentstore.Record) { r.Metadata.DeletedAt = new(time.Time) }},
		{"object deleted", func(r *attachmentstore.Record) { r.ObjectDeletedAt = new(time.Time) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := tenantAttemptSnapshot{attempt: leasestore.Attempt{OrganizationID: "org_example", PayloadObjectKey: ref.Key}, event: eventstore.Event{ProjectID: "prj_example"}}
			row := attachmentstore.Record{OrganizationID: "org_example", ObjectKey: ref.Key, UncompressedSize: ref.UncompressedSize,
				Codec: ref.Codec, SchemaVersion: 1, Kind: ref.Kind, Retention: ref.Retention,
				Metadata: attachment.Metadata{ID: newRunDetailReference(ref, "org_example", "").AttachmentID, ProjectID: "prj_example", SHA256: ref.SHA256, Size: ref.CompressedSize}}
			if test.change != nil {
				test.change(&row)
			}
			err := readTenantAttemptAttachmentPayload(t.Context(), attemptAttachmentReader{row: row}, &snapshot)
			if test.change == nil {
				if err != nil || snapshot.payload == nil || *snapshot.payload != ref {
					t.Fatalf("canonical reference = %+v: %v", snapshot.payload, err)
				}
			} else if !errors.Is(err, artifact.ErrIntegrity) || snapshot.payload != nil {
				t.Fatalf("invalid metadata accepted: reference=%+v error=%v", snapshot.payload, err)
			}
		})
	}
}

func TestNativeAttemptIdentityMismatchDiagnostics(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, field string
		change      func(*tenantAttemptSnapshot, *tracker.NativeRunData)
		expected    int64
		actual      int64
	}{
		{name: "matching"},
		{name: "cancelled", change: func(s *tenantAttemptSnapshot, _ *tracker.NativeRunData) { s.attempt.Sequence++ }},
		{name: "attempt", field: "attempt_id", change: func(_ *tenantAttemptSnapshot, r *tracker.NativeRunData) { r.AttemptID = "private-attempt" }},
		{name: "run", field: "run_id", change: func(_ *tenantAttemptSnapshot, r *tracker.NativeRunData) { r.RunID = "private-run" }},
		{name: "lease", field: "lease_id", change: func(_ *tenantAttemptSnapshot, r *tracker.NativeRunData) { r.LeaseID = "private-lease" }},
		{name: "fence", field: "fencing_token", expected: 7, actual: 8, change: func(_ *tenantAttemptSnapshot, r *tracker.NativeRunData) { r.FencingToken++ }},
		{name: "sequence", field: "sequence", expected: 4, actual: 2, change: func(s *tenantAttemptSnapshot, _ *tracker.NativeRunData) { s.attempt.Sequence += 2 }},
		{name: "release timestamp absent", field: "released_at", change: func(s *tenantAttemptSnapshot, _ *tracker.NativeRunData) {
			s.attempt.Sequence++
			s.lease.ReleasedAt = nil
		}},
		{name: "timestamp microsecond", field: "updated_at", change: func(s *tenantAttemptSnapshot, _ *tracker.NativeRunData) {
			s.attempt.Sequence++
			s.attempt.UpdatedAt = s.attempt.UpdatedAt.Add(time.Microsecond)
		}},
		{name: "timestamp nanosecond", field: "updated_at", change: func(s *tenantAttemptSnapshot, _ *tracker.NativeRunData) {
			s.attempt.Sequence++
			s.attempt.UpdatedAt = s.attempt.UpdatedAt.Add(time.Nanosecond)
		}},
		{name: "status", field: "sequence", expected: 3, actual: 2, change: func(s *tenantAttemptSnapshot, _ *tracker.NativeRunData) {
			s.attempt.Sequence++
			s.attempt.Status = "running"
		}},
		{name: "outcome", field: "sequence", expected: 3, actual: 2, change: func(s *tenantAttemptSnapshot, r *tracker.NativeRunData) {
			s.attempt.Sequence++
			r.Outcome = "private-outcome"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, hosted := range []bool{false, true} {
				t.Run(fmt.Sprintf("hosted=%t", hosted), func(t *testing.T) {
					now := time.Date(2026, time.November, 6, 12, 0, 0, 123456000, time.UTC)
					snapshot := tenantAttemptSnapshot{
						attempt: leasestore.Attempt{ID: "attempt_example", OrganizationID: "org_example", RunID: "run_example", Sequence: 2, Status: "cancelled", UpdatedAt: now},
						lease:   leasestore.Lease{LeaseID: "lease_example", FencingToken: 7, ReleasedAt: &now},
						event:   eventstore.Event{WorkItemID: "wi_example"},
					}
					run := tracker.NativeRunData{AttemptID: "attempt_example", RunID: "run_example", LeaseID: "lease_example", FencingToken: 7, Sequence: 2, CompletionBody: "private-content"}
					if test.change != nil {
						test.change(&snapshot, &run)
					}
					raw, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					details := &verifiedAttemptDetails{}
					ref, err := details.StoreDetail(t.Context(), "org_example", raw)
					if err != nil {
						t.Fatal(err)
					}
					snapshot.payload = &ref
					var logs bytes.Buffer
					var handler slog.Handler = slog.NewJSONHandler(&logs, nil)
					if hosted {
						handler = hostedLogHandler{output: handler}
					}
					service := &Service{config: Config{RunDetails: details, Logger: slog.New(handler)}}
					_, err = service.hydrateNativeAttemptTenant(t.Context(), snapshot, now)
					if test.field == "" {
						if err != nil || !nativeAttemptRunMatches(snapshot, run) {
							t.Fatalf("valid identity rejected: %v", err)
						}
						return
					}
					if !errors.Is(err, artifact.ErrIntegrity) || nativeAttemptRunMatches(snapshot, run) {
						t.Fatalf("identity integrity rejection = %v", err)
					}
					var mismatch *nativeAttemptIdentityMismatch
					if !errors.As(err, &mismatch) || mismatch.Field != test.field || mismatch.AttemptID != "attempt_example" {
						t.Fatalf("typed identity mismatch = %v", err)
					}
					if logs.Len() != 0 {
						t.Fatalf("logged before handling edge: %s", &logs)
					}
					response := httptest.NewRecorder()
					c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response)
					if err := service.nativeAPIError(c, fmt.Errorf("read attempt: %w", err)); err != nil {
						t.Fatal(err)
					}
					failure := requireNativeCode(t, response, http.StatusInternalServerError, "native_operation_failed")
					if failure.Message != "Native operation could not be completed" || strings.Contains(response.Body.String(), test.field) {
						t.Fatalf("internal diagnostics exposed: %s", response.Body)
					}
					var entry map[string]any
					decoder := json.NewDecoder(&logs)
					decoder.UseNumber()
					if err := decoder.Decode(&entry); err != nil {
						t.Fatal(err)
					}
					if entry["identity_field"] != test.field || entry["attempt_id"] != "attempt_example" || entry["work_item_id"] != "wi_example" || entry["error"] == nil {
						t.Fatalf("missing identity diagnostics: %+v", entry)
					}
					if test.field == "updated_at" {
						if entry["expected_value"] != json.Number(strconv.FormatInt(snapshot.attempt.UpdatedAt.UnixMicro(), 10)) || entry["actual_value"] != json.Number(strconv.FormatInt(now.UnixMicro(), 10)) || entry["timestamp_unit"] != "microseconds" {
							t.Fatalf("microsecond timestamp diagnostics: %+v", entry)
						}
					} else if test.field == "sequence" || test.field == "fencing_token" {
						if entry["expected_value"] != json.Number(strconv.FormatInt(test.expected, 10)) || entry["actual_value"] != json.Number(strconv.FormatInt(test.actual, 10)) {
							t.Fatalf("numeric diagnostic values: %+v", entry)
						}
					} else if entry["expected_value"] != nil || entry["actual_value"] != nil {
						t.Fatalf("non-numeric identity values logged: %+v", entry)
					}
					encoded, err := json.Marshal(entry)
					if err != nil || strings.Contains(string(encoded), "private-") {
						t.Fatalf("content in diagnostic: %s: %v", encoded, err)
					}
					if err := decoder.Decode(&entry); !errors.Is(err, io.EOF) {
						t.Fatalf("expected exactly one log: %v", err)
					}
				})
			}
		})
	}
}

type attemptSnapshotRepository struct {
	tenantstore.Repository
	tenantstore.SnapshotRepository
	beforeLatest func() error
	archiveErr   error
	hideReceipt  bool
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

func TestNativeAttemptImportedUsageCorrection(t *testing.T) {
	repository, _ := projectRecordPostgresWithAdmin(t, 4)
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

			terminal := start.Data
			terminal.Sequence, terminal.Outcome = 42, "succeeded"
			terminal.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: 100, Output: 20}}
			corrected := terminal
			corrected.Sequence = 43
			corrected.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: 120, Output: 25}}
			if !nativeUsageCorrection(terminal, corrected) {
				t.Fatal("fixture must reproduce a terminal usage correction")
			}
			raw, err := json.Marshal(corrected)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := details.StoreDetail(t.Context(), string(organization.ID), raw)
			if err != nil {
				t.Fatal(err)
			}
			historyRaw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: organization.ID, ProjectID: project.ID,
				AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, RecordedAt: now,
				Actor: tracker.Actor{Kind: "integration", PrincipalID: "example"}, Data: tracker.CollaborationData{Run: &terminal}})
			if err != nil {
				t.Fatal(err)
			}
			content := eventstore.NewContent(details)
			history, err := content.PrepareEvent(t.Context(), organization.ID, historyRaw)
			if err != nil {
				t.Fatal(err)
			}
			archive := attachmentstore.ArchiveRecord{OrganizationID: organization.ID, ProjectID: project.ID,
				ID: newRunDetailReference(ref, string(organization.ID), "").AttachmentID, Uploader: "test_hub_admin", CreatedAt: now, Reference: ref}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.AppendEvent(t.Context(), history); err != nil {
					return err
				}
				if _, err := w.PrepareAttachmentArchive(t.Context(), archive); err != nil {
					return err
				}
				return w.PublishAttachmentArchive(t.Context(), archive, issue.WorkItemID)
			}); err != nil {
				t.Fatal(err)
			}
			attempt := leasestore.Attempt{OrganizationID: organization.ID, ID: corrected.AttemptID, IssueID: lease.IssueID,
				LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: corrected.RunID, Sequence: 43, Status: "succeeded",
				WorkItemRevision: lease.WorkItemRevision, PayloadObjectKey: ref.Key, StartedAt: now.Add(-time.Minute), UpdatedAt: now}
			receipt, err := prepareCoreExecutionReceipt(t.Context(), content, organization.ID, strings.Repeat("a", 64), "core-attempt", attempt.ID, attempt)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: receipt}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), ref.Key)
				if !errors.Is(err, corestore.ErrNotFound) {
					return fmt.Errorf("import must preserve attachment-only payload metadata: %v", err)
				}
				latest, err := r.LatestRunEvent(t.Context(), project.ID, attempt.ID)
				if err != nil {
					return err
				}
				stored, err := content.Event(t.Context(), latest)
				if err == nil && (stored.Data.Run == nil || stored.Data.Run.Sequence != 42) {
					return errors.New("terminal history must remain at sequence 42")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, view := range []string{"single", "detail", "recovery"} {
				t.Run(view, func(t *testing.T) {
					var value tracker.NativeAttempt
					var err error
					if view == "single" {
						value, err = service.readNativeAttemptTenant(t.Context(), scope, string(issue.WorkItemID), attempt.ID)
					} else {
						var page nativeAttemptPage
						page, err = service.readNativeAttemptPageTenant(t.Context(), scope, string(issue.WorkItemID), url.Values{"view": {view}}, json.Marshal)
						if err == nil && len(page.Items) == 1 {
							value = page.Items[0]
						}
					}
					if err != nil {
						t.Fatalf("hydrate imported attempt 43 with terminal history 42: %v", err)
					}
					if value.Sequence != 43 || value.Outcome != "succeeded" || value.Status != "succeeded" || value.AttemptID != attempt.ID || len(value.Usage) != 1 || value.Usage[0].Input != 120 || value.Usage[0].Output != 25 {
						t.Fatalf("canonical corrected attempt = %+v", value)
					}
				})
			}
			t.Run("corrupt canonical payload", func(t *testing.T) {
				original := details.data[ref.Key]
				t.Cleanup(func() { details.data[ref.Key] = original })
				details.data[ref.Key] = []byte(`{"sequence":43}`)
				_, err := service.readNativeAttemptTenant(t.Context(), scope, string(issue.WorkItemID), attempt.ID)
				if !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("corrupt canonical payload fell back to history: %v", err)
				}
			})
		})
	}
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
				for _, transactional := range []bool{false, true} {
					var ref attachment.ArchiveReference
					if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
						attempt, err := r.Attempt(t.Context(), event.Data.AttemptID)
						if err != nil {
							return err
						}
						object, err := r.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), attempt.PayloadObjectKey)
						ref = object.Reference
						return err
					}); err != nil {
						t.Fatal(err)
					}
					state := func() [5]int64 {
						t.Helper()
						var values [5]int64
						err := admin.QueryRowContext(t.Context(), "SELECT sequence,(SELECT count(*) FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2),(SELECT count(*) FROM native_commands WHERE organization_id=$1),(SELECT count(*) FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1) FROM native_attempts WHERE organization_id=$1 AND id=$2", organization.ID, event.Data.AttemptID).Scan(&values[0], &values[1], &values[2], &values[3], &values[4])
						if err != nil {
							t.Fatal(err)
						}
						return values
					}
					before, applies := state(), participant.apply
					if _, err := admin.ExecContext(t.Context(), "DELETE FROM archive_objects WHERE organization_id=$1 AND object_key=$2", organization.ID, ref.Key); err != nil {
						t.Fatal(err)
					}
					wrapped.hideReceipt = transactional
					ctx := context.WithValue(t.Context(), attemptSnapshotContextKey{}, true)
					if _, err := service.appendNativeRunEventTenant(ctx, scope, string(second.WorkItemID), event); err != nil {
						t.Fatal(err)
					}
					if wrapped.hideReceipt || state() != before || participant.apply != applies {
						t.Fatal("request replay changed execution state or did not exercise receipt branch")
					}
					if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
						object, err := r.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), ref.Key)
						if err == nil && object.Reference != ref {
							return errors.New("request replay metadata differs")
						}
						return err
					}); err != nil {
						t.Fatalf("request replay transactional=%v: %v", transactional, err)
					}
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
					return service.readNativeAttempt(readContext, scope, string(issue.WorkItemID), start.Data.AttemptID)
				}},
				{"page", func() (tracker.NativeAttempt, error) {
					page, err := service.readNativeAttemptPage(readContext, scope, string(issue.WorkItemID), url.Values{}, json.Marshal)
					if err != nil {
						return tracker.NativeAttempt{}, err
					}
					if len(page.Items) != 1 {
						return tracker.NativeAttempt{}, fmt.Errorf("attempt page count = %d", len(page.Items))
					}
					return page.Items[0], nil
				}},
				{"initial_recovery", func() (tracker.NativeAttempt, error) {
					recovery, err := service.readNativeRecovery(readContext, scope, string(issue.WorkItemID))
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
					current, err := service.readNativeAttempt(readContext, scope, string(issue.WorkItemID), start.Data.AttemptID)
					if err != nil || current.Sequence != sequence {
						t.Fatalf("committed append sequence = %d, want %d: %v", current.Sequence, sequence, err)
					}
				})
			}

			if otherAttempt != "" {
				_, err := service.readNativeAttempt(readContext, scope, string(issue.WorkItemID), otherAttempt)
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
			if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET sequence=$1 WHERE organization_id=$2 AND id=$3", sequence, organization.ID, start.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "cancelled")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, reader := range readers {
				t.Run("lease_cancelled_"+reader.name, func(t *testing.T) {
					value, err := reader.read()
					if err != nil {
						t.Fatal(err)
					}
					if value.Status != "cancelled" || value.Current || value.Sequence != sequence || value.AttemptID != start.Data.AttemptID {
						t.Fatalf("cancelled attempt = %+v", value)
					}
				})
			}
			response := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/"+string(issue.WorkItemID)+"/attempts?view=recorded_blockers", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
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

func (r attemptSnapshotReader) Receipt(ctx context.Context, actor, operation, key string) (eventstore.Receipt, error) {
	if r.owner.hideReceipt {
		r.owner.hideReceipt = false
		return eventstore.Receipt{}, corestore.ErrNotFound
	}
	return r.Reader.Receipt(ctx, actor, operation, key)
}

func (r attemptSnapshotReader) RunEvent(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, id string) (eventstore.Event, error) {
	return r.Reader.(nativeRunEventReader).RunEvent(ctx, project, item, id)
}
