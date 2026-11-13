package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func sourceTransferPostgresFixture(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &verifiedAttemptDetails{}
	var logs bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	service := &Service{notifications: newNotificationBroker(), echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))}}
	provider := newHostedSecurityProvider()
	service.config.Hosted = &HostedConfig{OrganizationID: "org_first", Provider: provider}
	service.registerNativeRoutes(service.echo)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		member := auth.Membership{ID: "member_" + string(org), UserID: "user_" + string(org), OrganizationID: "provider_" + string(org), Status: "active"}
		member.Role.Slug = "owner"
		provider.members[member.ID] = member
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "project", WorkItemID: "item", Number: 1, Revision: 1, Profile: "native"}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(issue)
		if err != nil {
			t.Fatal(err)
		}
		version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "project", WorkItemID: "item", RecordID: "item", Revision: 1}, raw)
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "owner", Name: "Example owner", Hash: apikey.HashToken(string(org) + "_token"), Scope: "operator", NativeOnly: true, UserID: member.UserID, MembershipID: member.ID, KeyScope: "admin", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: member.UserID, Email: "owner@example.test", MembershipID: member.ID, Role: "owner", Active: true, PrincipalID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: "project", CanWrite: true, ManageRunner: true}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "owner", ProjectID: "project"}); err != nil {
				return err
			}
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "project", NativeID: "item", Number: 1, Title: "Example", Revision: 1, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("owner"), NativeCreatedAt: now, NativeUpdatedAt: now})
			if err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(t.Context(), version)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return service, admin
}

func TestPostgresSourceTransferSelectionWithoutLegacyDatabase(t *testing.T) {
	service, admin := sourceTransferPostgresFixture(t)
	for _, org := range []string{"org_first", "org_second"} {
		service.config.Hosted.OrganizationID = org
		for _, tc := range []struct {
			name     string
			revision tracker.Revision
			version  string
			status   int
		}{
			{"stale revision", 2, "version", http.StatusConflict},
			{"missing version", 1, "", http.StatusUnprocessableEntity},
			{"foreign version", 1, "foreign", http.StatusUnprocessableEntity},
		} {
			t.Run(org+"/"+tc.name, func(t *testing.T) {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Errorf("mounted source recovery panicked with nil legacy DB: %v", recovered)
					}
				}()
				request := sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: tc.name}, ExpectedRevision: tc.revision, VersionID: tc.version}
				response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+org+"/projects/project/work-items/item/source-recovery", org+"_token", request)
				requireNativeStatus(t, response, tc.status)
			})
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.config.Hosted.OrganizationID = string(org)
		seedSourceTransferAttempt(t, service, org)
		read := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/"+string(org)+"/projects/project/work-items/item/source-recovery", string(org)+"_token", nil)
		requireNativeStatus(t, read, http.StatusOK)
		var view sourceRecoveryView
		decodeHubResponse(t, read, &view)
		if view.VersionID != "version" || !view.Available || view.Quiesced {
			t.Fatalf("active source selection = %+v", view)
		}
		response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(org)+"/projects/project/work-items/item/source-recovery", string(org)+"_token", sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "active source"}, ExpectedRevision: 1, VersionID: "version"})
		requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		t.Run(string(org)+"/source projection", func(t *testing.T) {
			itemPath := "/api/v2/organizations/" + string(org) + "/projects/project/work-items/item"
			read := func(t *testing.T) sourceRecoveryView {
				t.Helper()
				response := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/source-recovery", string(org)+"_token", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var current sourceRecoveryView
				decodeHubResponse(t, response, &current)
				return current
			}
			parity := func(t *testing.T, current sourceRecoveryView) {
				t.Helper()
				response := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/explanation", string(org)+"_token", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var explanation struct {
					Runtime *tracker.NativeRuntimeEvidence `json:"native_runtime"`
				}
				decodeHubResponse(t, response, &explanation)
				if explanation.Runtime == nil || explanation.Runtime.SourceRecovery == nil {
					t.Fatal("existing explanation lost source ownership or waiting reason")
				}
				got := explanation.Runtime.SourceRecovery
				if slices.Contains(explanation.Runtime.Unavailable, "source_recovery_projection") {
					t.Fatal("successful source projection retained unavailable marker")
				}
				if current.SourceMachineID != "machine" || current.SourceRunnerID != "" || current.SourceRunnerName != "example" || current.VersionID != "version" {
					t.Fatalf("source ownership differs from seeded owners: %+v", current)
				}
				wantReason := "Finish or cancel work on the source runner and release its lease before transfer; expiry or an unreachable owner does not prove quiescence"
				if current.Quiesced {
					wantReason = "Verified retained source can be restored before continuing; workflow, human and delivery holds remain in force"
				}
				if current.Reason != wantReason {
					t.Fatalf("source reason=%q want=%q", current.Reason, wantReason)
				}
				if got.AttemptID != current.AttemptID || got.SourceRunnerName != current.SourceRunnerName || got.Quiesced != current.Quiesced || got.Reason != current.Reason || got.VersionID != current.VersionID || got.HeadSHA != current.HeadSHA || got.BaseSHA != current.BaseSHA || got.Available != current.Available || len(got.Destinations) != 0 || got.DestinationsNext != "" {
					t.Fatalf("explanation source projection differs: got=%+v want=%+v", got, current)
				}
			}
			parity(t, view)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				attempt, err := w.Attempt(t.Context(), sourceTransferAttemptID)
				if err != nil {
					return err
				}
				_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken}, "completed")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			seed := func(id string, checkpoint tracker.NativeCheckpoint) (leasestore.Attempt, attachment.ArchiveReference) {
				t.Helper()
				var lease leasestore.Lease
				if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), "project", "item")
					if err != nil {
						return err
					}
					lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine", SessionID: id, LeaseID: "lease_" + id, TTL: time.Hour})
					if err != nil {
						return err
					}
					return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: org, ProjectID: "project", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: lease.AcquiredAt})
				}); err != nil {
					t.Fatal(err)
				}
				content := eventstore.NewContent(service.runDetailStore())
				raw, err := json.Marshal(checkpointPayload{Checkpoint: &checkpoint})
				if err != nil {
					t.Fatal(err)
				}
				checkpointRef, err := service.runDetailStore().StoreDetail(t.Context(), string(org), raw)
				if err != nil {
					t.Fatal(err)
				}
				var attempts []leasestore.AttemptEvent
				var events []eventstore.Event
				for sequence, kind := range []string{"run.started", "run.checkpointed", "run.finished"} {
					status := "running"
					run := tracker.NativeRunData{AttemptID: id, RunID: "run_" + strings.TrimPrefix(id, "attempt_"), LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: "machine", PolicyID: hubTestPolicy().ID, Sequence: int64(sequence + 1), Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "example", Model: "example"}}
					if sequence == 1 {
						run.Handoff = &checkpoint
					}
					if sequence == 2 {
						status, run.Outcome = "succeeded", "succeeded"
					}
					raw, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					ref, err := service.runDetailStore().StoreDetail(t.Context(), string(org), raw)
					if err != nil {
						t.Fatal(err)
					}
					event := leasestore.AttemptEvent{ID: id, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: run.RunID, Sequence: run.Sequence, Status: status, StartedAt: lease.AcquiredAt, PayloadObjectKey: ref.Key, PayloadReference: ref}
					if sequence == 1 {
						event.CheckpointObjectKey, event.CheckpointReference, event.CheckpointPreservesSource = &checkpointRef.Key, checkpointRef, new(checkpoint.PreservesSource())
					}
					attempts = append(attempts, event)
					raw, err = json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("event_%s_%d", id, sequence), OrganizationID: org, ProjectID: "project", AggregateID: "item", Type: kind, Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine"}, SchemaVersion: 1, RecordedAt: lease.AcquiredAt.Add(time.Duration(sequence) * time.Microsecond), Data: tracker.CollaborationData{Run: &run}})
					if err != nil {
						t.Fatal(err)
					}
					stored, err := content.PrepareEvent(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					events = append(events, stored)
				}
				var saved leasestore.Attempt
				if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					for i, event := range attempts {
						var err error
						saved, err = w.RecordAttempt(t.Context(), event)
						if err != nil {
							return err
						}
						if _, err := w.AppendEvent(t.Context(), events[i]); err != nil {
							return err
						}
					}
					_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return saved, checkpointRef
			}
			preserving := tracker.NativeCheckpoint{Resume: "fresh_checkout", WorktreeState: "unpushed", HeadSHA: view.HeadSHA, Availability: "available", Storage: "local_only", ExternalEffect: "none", EffectState: "none"}
			source, sourceRef := seed("attempt_"+strings.Repeat("f", 32), preserving)
			clean := preserving
			clean.WorktreeState = "clean"
			var latest leasestore.Attempt
			var latestRef attachment.ArchiveReference
			for i := range 101 {
				latest, latestRef = seed(fmt.Sprintf("attempt_%032x", i), clean)
			}
			current := read(t)
			if current.AttemptID != source.ID || current.VersionID != "version" || !current.Available || !current.Quiesced || current.HeadSHA != view.HeadSHA {
				t.Fatalf("newer clean checkpoints hid exact source: %+v", current)
			}
			parity(t, current)
			for _, unknown := range []struct {
				name       string
				attempt    leasestore.Attempt
				reference  attachment.ArchiveReference
				checkpoint tracker.NativeCheckpoint
			}{
				{"newer unclassified checkpoint", latest, latestRef, clean},
				{"unclassified preserving checkpoint", source, sourceRef, preserving},
			} {
				t.Run(unknown.name, func(t *testing.T) {
					t.Cleanup(func() {
						ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
						defer cancel()
						if err := service.config.TenantRepository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
							owner, ok := w.(leasestore.CheckpointSourceWriter)
							if !ok {
								return hubapi.NativeRunEventStorageUnavailable()
							}
							return owner.SetCheckpointSource(ctx, leasestore.CheckpointSourceFact{AttemptID: unknown.attempt.ID, IssueID: unknown.attempt.IssueID, Sequence: unknown.attempt.Sequence, CheckpointObjectKey: unknown.reference.Key, PreservesSource: unknown.checkpoint.PreservesSource()})
						}); err != nil {
							t.Errorf("restore classified checkpoint: %v", err)
						}
					})
					result, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET checkpoint_preserves_source=NULL WHERE organization_id=$1 AND id=$2 AND checkpoint_object_key=$3 AND sequence=$4`, org, unknown.attempt.ID, unknown.reference.Key, unknown.attempt.Sequence)
					if err != nil {
						t.Fatal(err)
					}
					if count, err := result.RowsAffected(); err != nil || count != 1 {
						t.Fatalf("legacy unknown row count=%d err=%v", count, err)
					}
					response := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/source-recovery", string(org)+"_token", nil)
					requireNativeStatus(t, response, http.StatusServiceUnavailable)
					var failure hubapi.APIErrorResponse
					decodeHubResponse(t, response, &failure)
					if failure.Code != "not_ready" {
						t.Fatalf("unknown checkpoint misclassified: code=%s", failure.Code)
					}
					response = performHubAPIRequest(t, service, http.MethodPost, itemPath+"/source-recovery", string(org)+"_token", sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: unknown.name}, ExpectedRevision: current.Revision, VersionID: current.VersionID})
					requireNativeStatus(t, response, http.StatusServiceUnavailable)
					decodeHubResponse(t, response, &failure)
					if failure.Code != "not_ready" {
						t.Fatalf("unknown transfer misclassified: code=%s", failure.Code)
					}
					response = performHubAPIRequest(t, service, http.MethodGet, itemPath+"/explanation", string(org)+"_token", nil)
					requireNativeStatus(t, response, http.StatusOK)
					var explanation struct {
						Runtime *tracker.NativeRuntimeEvidence `json:"native_runtime"`
					}
					decodeHubResponse(t, response, &explanation)
					if explanation.Runtime == nil || explanation.Runtime.SourceRecovery != nil || !slices.Contains(explanation.Runtime.Unavailable, "source_recovery_projection") || explanation.Runtime.Issue.WorkItemID != "item" || explanation.Runtime.Attempt == nil || explanation.Runtime.Attempt.AttemptID != latest.ID {
						t.Fatalf("unknown optional source destroyed runtime evidence: %+v", explanation.Runtime)
					}
					response = performHubAPIRequest(t, service, http.MethodGet, itemPath+"/runtime", string(org)+"_token", nil)
					requireNativeStatus(t, response, http.StatusOK)
					var runtime tracker.NativeRuntimeEvidence
					decodeHubResponse(t, response, &runtime)
					if runtime.SourceRecovery != nil || runtime.Attempt == nil || runtime.Attempt.AttemptID != latest.ID {
						t.Fatalf("ordinary runtime acquired source dependency: %+v", runtime)
					}

				})
				parity(t, read(t))
			}
			var sourceKey string
			if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				version, err := r.Version(t.Context(), "change", "version")
				sourceKey = version.SourceObjectKey
				return err
			}); err != nil {
				t.Fatal(err)
			}
			originalStore := service.config.RunDetails
			for _, outage := range []struct {
				name   string
				err    error
				status int
				code   string
			}{
				{"storage", artifact.ErrStorage, http.StatusServiceUnavailable, "storage_unreachable"},
				{"unclassified", errors.New("temporary source read failed"), http.StatusInternalServerError, "native_operation_failed"},
			} {
				t.Run("source outage/"+outage.name, func(t *testing.T) {
					intercepted := &changeReadInterceptionStore{DetailStore: originalStore, key: sourceKey}
					service.config.RunDetails = struct {
						attachment.DetailStore
						attachment.CanonicalDetailStore
					}{intercepted, originalStore.(attachment.CanonicalDetailStore)}
					t.Cleanup(func() { service.config.RunDetails = originalStore })
					fail := func(context.Context) error { return outage.err }
					intercepted.afterRead = fail
					response := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/runtime", string(org)+"_token", nil)
					requireNativeStatus(t, response, http.StatusOK)
					var runtime tracker.NativeRuntimeEvidence
					decodeHubResponse(t, response, &runtime)
					if runtime.SourceRecovery != nil || runtime.Attempt == nil || runtime.Attempt.AttemptID != latest.ID || intercepted.afterRead == nil {
						t.Fatal("ordinary runtime read the optional source bundle or lost its attempt")
					}
					response = performHubAPIRequest(t, service, http.MethodGet, itemPath+"/explanation", string(org)+"_token", nil)
					requireNativeStatus(t, response, http.StatusOK)
					var explanation struct {
						Runtime *tracker.NativeRuntimeEvidence `json:"native_runtime"`
					}
					decodeHubResponse(t, response, &explanation)
					if explanation.Runtime == nil || explanation.Runtime.SourceRecovery != nil || !slices.Contains(explanation.Runtime.Unavailable, "source_recovery_projection") || explanation.Runtime.Attempt == nil || explanation.Runtime.Attempt.AttemptID != latest.ID || intercepted.afterRead != nil {
						t.Fatalf("source outage changed authoritative evidence: %+v", explanation.Runtime)
					}
					intercepted.afterRead = fail
					response = performHubAPIRequest(t, service, http.MethodGet, itemPath+"/source-recovery", string(org)+"_token", nil)
					requireNativeStatus(t, response, outage.status)
					var failure hubapi.APIErrorResponse
					decodeHubResponse(t, response, &failure)
					if failure.Code != outage.code || strings.Contains(response.Body.String(), "No verified Hub source") || intercepted.afterRead != nil {
						t.Fatalf("source outage became absent source: code=%s", failure.Code)
					}
				})
			}
			for _, condition := range []string{"missing", "corrupt"} {
				t.Run("source object/"+condition, func(t *testing.T) {
					details := originalStore.(*verifiedAttemptDetails)
					details.mu.Lock()
					original := bytes.Clone(details.data[sourceKey])
					if len(original) == 0 {
						details.mu.Unlock()
						t.Fatal("canonical source object missing before removal")
					}
					delete(details.data, sourceKey)
					if condition == "corrupt" {
						details.data[sourceKey] = []byte("invalid source bytes")
					}
					details.mu.Unlock()
					t.Cleanup(func() { details.mu.Lock(); details.data[sourceKey] = original; details.mu.Unlock() })
					response := performHubAPIRequest(t, service, http.MethodGet, itemPath+"/source-recovery", string(org)+"_token", nil)
					requireNativeStatus(t, response, http.StatusOK)
					var unavailable sourceRecoveryView
					decodeHubResponse(t, response, &unavailable)
					if unavailable.Available || unavailable.AttemptID != source.ID || unavailable.Reason != "No verified Hub source is available for recovery" {
						t.Fatalf("invalid source was accepted or lost ownership: %+v", unavailable)
					}
					response = performHubAPIRequest(t, service, http.MethodGet, itemPath+"/explanation", string(org)+"_token", nil)
					requireNativeStatus(t, response, http.StatusOK)
					var explanation struct {
						Runtime *tracker.NativeRuntimeEvidence `json:"native_runtime"`
					}
					decodeHubResponse(t, response, &explanation)
					if explanation.Runtime == nil || explanation.Runtime.SourceRecovery != nil || !slices.Contains(explanation.Runtime.Unavailable, "source_recovery_projection") || explanation.Runtime.Attempt == nil || explanation.Runtime.Attempt.AttemptID != latest.ID {
						t.Fatalf("unavailable source destroyed remaining evidence: %+v", explanation.Runtime)
					}
				})
			}
			parity(t, read(t))

		})
	}

	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/org_second/projects/project/work-items/item/source-recovery", "org_first_token", sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "foreign"}, ExpectedRevision: 1})
	if response.Code < 400 || response.Code >= 500 {
		t.Fatalf("cross-tenant transfer status=%d", response.Code)
	}
}

const (
	sourceTransferAttemptID = "attempt_0123456789abcdef0123456789abcdef"
	sourceTransferRunID     = "run_0123456789abcdef0123456789abcdef"
)

func seedSourceTransferAttempt(t *testing.T, s *Service, org tracker.OrganizationID) leasestore.Lease {
	t.Helper()
	now := s.config.now()
	content := eventstore.NewContent(s.runDetailStore())
	marshal := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	policy := hubTestPolicy()
	approval, err := content.PrepareProjectPolicy(t.Context(), org, policy)
	if err != nil {
		t.Fatal(err)
	}
	approval.ProjectID, approval.ApprovedAt, approval.ApprovedBy = "project", now, "owner"
	input := changeTestInput()
	input.PolicyID, input.AttemptID = policy.ID, sourceTransferAttemptID
	bundle := changeTestSource(t, &input)
	version, err := content.PrepareVersion(t.Context(), org, marshal(tracker.ChangeVersion{ID: "version", ChangeID: "change", Number: 1, ChangeVersionInput: input, Policy: policy, CreatedAt: now}))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := content.PrepareSourceBundle(t.Context(), org, bundle)
	if err != nil {
		t.Fatal(err)
	}
	version.SourceObjectKey, version.SourceSHA256, version.SourceBytes, version.SourceContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	change, err := content.PrepareChange(t.Context(), org, marshal(tracker.ChangeRequest{ID: "change", OrganizationID: org, ProjectID: "project", WorkItemID: "item", Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}))
	if err != nil {
		t.Fatal(err)
	}
	var lease leasestore.Lease
	err = s.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.ApproveProjectPolicy(t.Context(), approval, ""); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "machine", Hostname: "example", Capacity: 2, Version: "example"}); err != nil {
			return err
		}
		item, err := w.WorkItem(t.Context(), "project", "item")
		if err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine", SessionID: "session_" + string(org), LeaseID: "lease_" + string(org), TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: org, ProjectID: "project", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: policy.ID, BoundAt: lease.AcquiredAt}); err != nil {
			return err
		}
		run := tracker.NativeRunData{PolicyID: policy.ID, AttemptID: sourceTransferAttemptID, RunID: sourceTransferRunID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: "machine", Sequence: 1}
		event, err := content.PrepareEvent(t.Context(), org, marshal(tracker.CollaborationEvent{ID: "event", OrganizationID: org, ProjectID: "project", AggregateID: "item", Type: "run.observed", Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine"}, SchemaVersion: 1, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}))
		if err != nil {
			return err
		}
		canonical, err := s.runDetailStore().StoreDetail(t.Context(), string(org), marshal(run))
		if err != nil {
			return err
		}
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: sourceTransferAttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: sourceTransferRunID, Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: canonical.Key}); err != nil {
			return err
		}
		run.Sequence, run.Outcome = 2, "succeeded"
		if org == "org_second" {
			run.Outcome = "failed"
		}
		canonical, err = s.runDetailStore().StoreDetail(t.Context(), string(org), marshal(run))
		if err != nil {
			return err
		}
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: sourceTransferAttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: sourceTransferRunID, Sequence: 2, Status: run.Outcome, PayloadObjectKey: canonical.Key}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		change, err = w.CreateChange(t.Context(), change)
		if err != nil {
			return err
		}
		if _, err := w.CreateVersion(t.Context(), version); err != nil {
			return err
		}
		change.CurrentVersionID = new("version")
		if _, err = w.UpdateChange(t.Context(), change); err != nil {
			return err
		}
		return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "project", WorkItemID: "item", ChangeID: "change"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestPostgresSourceTransferCommitsAndReplays(t *testing.T) {
	s, _ := sourceTransferPostgresFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			s.config.Hosted.OrganizationID = string(org)
			lease := seedSourceTransferAttempt(t, s, org)
			if err := s.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "released"); err != nil {
					return err
				}
				now := s.config.now()
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "destination", Name: "Example destination", Hash: apikey.HashToken(string(org) + "_destination"), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "destination", ProjectID: "project"}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "enrollment", RunnerID: "destination", MachineID: "machine", TokenHash: apikey.HashToken(string(org) + "_enrollment"), Scope: "projects", ProjectIDs: []tracker.ProjectID{"project"}, Operations: []string{runnerauth.Claim}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "owner"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment", "destination", "machine", now); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: "destination", MachineID: "machine", TokenID: "destination", EnrollmentID: "enrollment", Scope: "projects", ProjectIDs: []tracker.ProjectID{"project"}, Operations: []string{runnerauth.Claim}, OS: "linux", IsolationTier: isolation.NativeTrusted, CapacityLimit: 2, ReportedCapacity: 2, CreatedAt: now, LastHeartbeatAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
					return err
				}
				return w.ReplaceRunnerBackendIsolation(t.Context(), "destination", isolation.Report{"example": {isolation.NativeTrusted}})
			}); err != nil {
				t.Fatal(err)
			}
			path := "/api/v2/organizations/" + string(org) + "/projects/project/work-items/item/source-recovery"
			request := sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "transfer"}, ExpectedRevision: 1, VersionID: "version", DestinationRunnerID: "destination"}
			var canonicalKey string
			if err := s.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				attempt, err := r.Attempt(t.Context(), sourceTransferAttemptID)
				if err != nil {
					return err
				}
				canonicalKey = attempt.PayloadObjectKey
				if _, err := r.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), canonicalKey); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("fixture must lack archive metadata: %v", err)
				}
				id := newRunDetailReference(attachment.ArchiveReference{Key: canonicalKey}, string(org), "").AttachmentID
				if _, err := r.Attachment(t.Context(), "project", id); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("fixture must lack attachment metadata: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			details := s.config.RunDetails.(*verifiedAttemptDetails)
			for _, scenario := range []string{"missing", "corrupt"} {
				t.Run(scenario, func(t *testing.T) {
					original := details.data[canonicalKey]
					t.Cleanup(func() { details.data[canonicalKey] = original })
					delete(details.data, canonicalKey)
					if scenario == "corrupt" {
						details.data[canonicalKey] = []byte(`{"sequence":2}`)
					}
					refused := request
					refused.IdempotencyKey = scenario
					response := performHubAPIRequest(t, s, http.MethodPost, path, string(org)+"_token", refused)
					if scenario == "corrupt" {
						requireNativeStatus(t, response, http.StatusServiceUnavailable)
						if !strings.Contains(response.Body.String(), "archive_invalid") {
							t.Fatalf("corrupt canonical attempt must be refused as an invalid archive: %s", response.Body)
						}
					} else {
						requireNativeStatus(t, response, http.StatusInternalServerError)
					}
					if err := s.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.WorkItem(t.Context(), "project", "item")
						if err == nil && (row.Revision != 1 || row.DispatchGeneration != 0 || row.RecoveryRunnerID != "" || row.RecoveryVersionID != "") {
							t.Errorf("refused transfer changed work item: %+v", row)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
			responses := make([]*httptest.ResponseRecorder, 2)
			var requests sync.WaitGroup
			for i := range responses {
				req := newHubAPIRequest(t, http.MethodPost, path, string(org)+"_token", request)
				requests.Go(func() { responses[i] = httptest.NewRecorder(); s.Handler().ServeHTTP(responses[i], req) })
			}
			requests.Wait()
			response := responses[0]
			requireNativeStatus(t, responses[1], http.StatusOK)
			if response.Body.String() != responses[1].Body.String() {
				t.Fatal("concurrent transfer changed response")
			}
			requireNativeStatus(t, response, http.StatusOK)
			var result sourceRecoveryView
			decodeHubResponse(t, response, &result)
			if result.DestinationRunnerID != "destination" || result.Revision != 2 || result.VersionID != "version" || !result.Quiesced || !result.Available {
				t.Fatalf("transfer = %+v", result)
			}
			recoveryResponse := performHubAPIRequest(t, s, http.MethodGet, strings.TrimSuffix(path, "/source-recovery")+"?view=recovery", string(org)+"_token", nil)
			requireNativeStatus(t, recoveryResponse, http.StatusOK)
			var recovered tracker.NativeRecovery
			decodeHubResponse(t, recoveryResponse, &recovered)
			if recovered.SourceVersionID != "version" {
				t.Fatalf("continuation source = %q", recovered.SourceVersionID)
			}
			if len(recovered.Attempts) != 1 || recovered.Attempts[0].Sequence != 2 || recovered.Attempts[0].Outcome == "" {
				t.Fatalf("continuation lost canonical attempt: %+v", recovered.Attempts)
			}
			replay := performHubAPIRequest(t, s, http.MethodPost, path, string(org)+"_token", request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("replay changed committed response")
			}
			for _, tc := range []struct {
				key, version, destination string
				revision                  tracker.Revision
				status                    int
			}{
				{"transfer", "other", "", 1, http.StatusConflict},
				{"stale", "version", "", 1, http.StatusConflict},
				{"destination", "version", "missing", 2, http.StatusUnprocessableEntity},
			} {
				request.IdempotencyKey, request.VersionID, request.DestinationRunnerID, request.ExpectedRevision = tc.key, tc.version, tc.destination, tc.revision
				requireNativeStatus(t, performHubAPIRequest(t, s, http.MethodPost, path, string(org)+"_token", request), tc.status)
			}
			if err := s.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), "project", "item")
				if err == nil && (row.Revision != 2 || row.DispatchGeneration != 1 || row.RecoveryRunnerID != "destination" || row.RecoveryVersionID != "version") {
					t.Errorf("transfer or replay mutated revision/dispatch more than once: %+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			provider := s.config.Hosted.Provider.(*hostedSecurityProvider)
			membership := provider.members["member_"+string(org)]
			membership.Role.Slug = "member"
			provider.members[membership.ID] = membership
			if err := s.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: membership.UserID, ProjectID: "project", CanWrite: true})
			}); err != nil {
				t.Fatal(err)
			}
			request = sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "transfer"}, ExpectedRevision: 1, VersionID: "version", DestinationRunnerID: "destination"}
			requireNativeStatus(t, performHubAPIRequest(t, s, http.MethodPost, path, string(org)+"_token", request), http.StatusForbidden)
		})
	}
}
