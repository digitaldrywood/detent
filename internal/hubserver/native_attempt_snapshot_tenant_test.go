package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
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
	details := &memoryRunDetails{}
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
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: "machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
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
				if !errors.Is(err, corestore.ErrNotFound) {
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
