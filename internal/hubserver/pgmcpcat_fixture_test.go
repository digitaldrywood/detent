package hubserver

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func pgmcpcatSeed(t *testing.T, s *Service, org tracker.OrganizationID) {
	t.Helper()
	now := s.config.now().UTC()
	envelope, err := s.config.SecretKeys.Seal(bytes.Repeat([]byte{0x36}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "set", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"project", "compat"} {
		profile := "native"
		item, state := "wi_item", "Todo"
		if project == "compat" {
			profile, item = "github_compatible", "wi_compat_item"
		}
		if err := s.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: tracker.ProjectID(project), Name: "Example " + string(org) + project, Slug: project, Profile: profile}, States: humanWorkflowFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, ProjectID: tracker.ProjectID(project), UserID: string(org) + "_owner", CanWrite: true, ManageRunner: true}); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, ProjectID: tracker.ProjectID(project), TokenID: string(org) + "_owner"})
		}); err != nil {
			t.Fatal(err)
		}
		ids := []string{item}
		if project == "project" {
			ids = append(ids, "wi_verify_item", "wi_reopen_item")
		}
		for number, id := range ids {
			verification := (*tracker.NativeVerification)(nil)
			if id == "wi_verify_item" || id == "wi_reopen_item" {
				state = "Done"
				verification = &tracker.NativeVerification{State: tracker.VerificationPending}
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: tracker.ProjectID(project), WorkItemID: tracker.NativeWorkItemID(id), Number: number + 1, Revision: 1, Profile: profile}, Title: string(org), Body: workItemRecordBody, IssueContract: &issuecontract.State{ConfirmedSections: config.IssueContractSectionDigests(workItemRecordBody), RecordedAt: now}, State: state, Actor: tracker.Actor{Kind: "human", PrincipalID: string(org) + "_owner"}, Verification: verification, CreatedAt: now, UpdatedAt: now}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			version, err := eventstore.NewContent(s.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: tracker.ProjectID(project), WorkItemID: tracker.NativeWorkItemID(id), RecordID: id, Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			document, err := searchstore.NewContent(s.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: org, ProjectID: tracker.ProjectID(project), WorkItemID: tracker.NativeWorkItemID(id), ID: "issue:" + id, SourceID: id, Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: now}, searchstore.Text{Title: string(org)})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: tracker.ProjectID(project), NativeID: tracker.NativeWorkItemID(id), Number: int64(number + 1), Title: string(org), Revision: 1, WorkflowStateName: &state, ActorKind: new("human"), ActorPrincipalID: new(string(org) + "_owner"), NativeCreatedAt: now, NativeUpdatedAt: now})
				if err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
					return err
				}
				if err := w.PutSearch(t.Context(), document, 0); err != nil {
					return err
				}
				return w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: org, ProjectID: tracker.ProjectID(project), WorkItemID: tracker.NativeWorkItemID(id), Author: string(org) + "_owner"})
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func pgmcpcatPayload(raw string) string {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &envelope) == nil && len(envelope.Data) > 0 {
		return string(envelope.Data)
	}
	return raw
}
