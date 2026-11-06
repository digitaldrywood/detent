package hubserver

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func sourceTransferPostgresFixture(t *testing.T) *Service {
	t.Helper()
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
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
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), Local: true, CreatedAt: now}); err != nil {
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
	return service
}

func TestPostgresSourceTransferSelectionWithoutLegacyDatabase(t *testing.T) {
	service := sourceTransferPostgresFixture(t)
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
	}

	response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/org_second/projects/project/work-items/item/source-recovery", "org_first_token", sourceTransferRequest{Mutation: tracker.Mutation{IdempotencyKey: "foreign"}, ExpectedRevision: 1})
	if response.Code < 400 || response.Code >= 500 {
		t.Fatalf("cross-tenant transfer status=%d", response.Code)
	}
}

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
	input.PolicyID, input.AttemptID = policy.ID, "attempt"
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
		run := tracker.NativeRunData{AttemptID: "attempt", RunID: "run", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: "machine", Sequence: 1, Outcome: "succeeded"}
		event, err := content.PrepareEvent(t.Context(), org, marshal(tracker.CollaborationEvent{ID: "event", OrganizationID: org, ProjectID: "project", AggregateID: "item", Type: "run.finished", Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine"}, SchemaVersion: 1, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}))
		if err != nil {
			return err
		}
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run", Sequence: 1, Status: "succeeded", StartedAt: now, PayloadObjectKey: event.PayloadObjectKey}); err != nil {
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
	s := sourceTransferPostgresFixture(t)
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
