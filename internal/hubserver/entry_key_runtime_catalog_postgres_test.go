package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestPostgresEntryKeyRuntimeCatalog(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := pilotHostedPlans()
		cfg.Hosted.Plans = &plans
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			org := cfg.Hosted.OrganizationID
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
	})
	keyClaims := func(c *cloudassert.Claims) {
		c.Kind = cloudassert.KindKey
		c.ProviderOrganization = "provider_" + c.Audience
		c.Key = &apikey.KeyAuthority{ID: "key_" + c.Subject, CredentialBinding: strings.Repeat("b", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}
	}
	for org, service := range f.services {
		organization := tracker.OrganizationID(org)
		now := service.config.now().UTC()
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: "project", WorkItemID: "item", Number: 1, Revision: 1, Profile: "native"}, Title: org, State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: org + "_owner"}, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(issue)
		if err != nil {
			t.Fatal(err)
		}
		version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: "project", WorkItemID: "item", RecordID: "item", Revision: 1}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project", Name: "Example project", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, ProjectID: "project", UserID: org + "_owner", CanWrite: true, ManageRunner: true}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, ProjectID: "project", TokenID: org + "_owner"}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: "project", NativeID: "item", Number: 1, Title: org, Revision: 1, WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new(org + "_owner"), NativeCreatedAt: now, NativeUpdatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		lease := seedEntryRuntimeAttempt(t, service, organization)
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "released")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct{ name, arguments, want string }{
			{operatortool.WorkAttemptReceipt, `"reference":"#1","native_attempt_id":"attempt_0123456789abcdef0123456789abcdef"`, `"attempt_id":"attempt_0123456789abcdef0123456789abcdef"`},
			{operatortool.GitHubScopeTimings, `"reference":"#1","native_attempt_id":"attempt_0123456789abcdef0123456789abcdef","runner_id":"runner_catalog"`, `"scope":"` + org + `"`},
			{operatortool.BoardSessionHistory, `"reference":"#1","native_attempt_id":"attempt_0123456789abcdef0123456789abcdef"`, `"id":"` + org + `"`},
			{operatortool.ExplainItem, `"reference":"#1","include":["summary"]`, `"issue_id":"item"`},
			{operatortool.CreateRunnerEnrollment, `"request_id":"entry-enrollment","enrollment":{"project_ids":["project"],"operations":["read"],"ttl_seconds":60}`, `"id":"enrollment_`},
			{operatortool.CreateRunnerEnrollment, `"request_id":"entry-enrollment-projects","enrollment":{"scope":"projects","project_ids":["project"],"operations":["read"],"ttl_seconds":60}`, `"scope":"projects"`},
			{operatortool.CreateRunnerEnrollment, `"request_id":"entry-enrollment-organization","enrollment":{"scope":"organization","project_ids":[],"operations":["read"],"ttl_seconds":60}`, `"scope":"organization"`},
			{operatortool.MarkUrgentRunnerUpdate, `"request_id":"entry-urgent","change":{"expected_revision":0,"version":"1.2.3"}`, `"version":"1.2.3"`},
			{operatortool.TransferItem, `"identifier":"#1","expected_revision":"1","version_id":"version","request_id":"entry-transfer"`, `"quiesced":true`},
		} {
			t.Run(org+"/"+test.name, func(t *testing.T) {
				catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, keyClaims)
				requireNativeStatus(t, catalog, http.StatusOK)
				var definitions []operatortool.Definition
				decodeHubResponse(t, catalog, &definitions)
				if !entryKeyHasDefinition(definitions, test.name) {
					t.Errorf("%s missing from signed catalog", test.name)
				}
				body := fmt.Sprintf(`{"name":%q,"arguments":{"project_id":"project",%s},"connection_id":"runtime_catalog"}`, test.name, test.arguments)
				response := f.request(t, org, "/internal/v1/keys/call", body, keyClaims)
				requireNativeStatus(t, response, http.StatusOK)
				if !strings.Contains(response.Body.String(), test.want) {
					t.Fatalf("call omitted persisted evidence %s: %s", test.want, response.Body.String())
				}
			})
		}
	}
}

func seedEntryRuntimeAttempt(t *testing.T, s *Service, org tracker.OrganizationID) leasestore.Lease {
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
	input.PolicyID, input.AttemptID = policy.ID, "attempt_0123456789abcdef0123456789abcdef"
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
		if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: org, ProjectID: "project", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: policy.ID, BoundAt: now}); err != nil {
			return err
		}
		run := tracker.NativeRunData{AttemptID: "attempt_0123456789abcdef0123456789abcdef", RunID: "run", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: "machine", RunnerID: "runner_catalog", PolicyID: policy.ID, Sequence: 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 1, Generation: 1, HeartbeatAt: now, Phase: "completed", Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 1, Generation: 1, Coverage: "partial", AsOf: now, Spans: []workflowmetrics.ActivitySpan{{ID: string(org), Kind: "tool", Outcome: "succeeded", StartedAt: now}}}, GitHub: &tracker.NativeGitHubScope{Scope: string(org), ObservedAt: now}}}
		event, err := content.PrepareEvent(t.Context(), org, marshal(tracker.CollaborationEvent{ID: "event", OrganizationID: org, ProjectID: "project", AggregateID: "item", Type: "run.finished", Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine"}, SchemaVersion: 1, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}))
		if err != nil {
			return err
		}

		payload, err := s.runDetailStore().StoreDetail(t.Context(), string(org), marshal(run))
		if err != nil {
			return err
		}
		archive := attachmentstore.ArchiveRecord{OrganizationID: org, ProjectID: "project", ID: newRunDetailReference(payload, string(org), "").AttachmentID, Uploader: string(org) + "_owner", CreatedAt: now, Reference: payload}
		if _, err := w.PrepareAttachmentArchive(t.Context(), archive); err != nil {
			return err
		}
		if err := w.PublishAttachmentArchive(t.Context(), archive, "item"); err != nil {
			return err
		}
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt_0123456789abcdef0123456789abcdef", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run", Sequence: 1, Status: "succeeded", StartedAt: now, PayloadObjectKey: payload.Key}); err != nil {
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
