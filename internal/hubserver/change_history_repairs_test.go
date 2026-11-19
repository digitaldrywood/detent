package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRecoveryUnknownHistoryBudgetPreservesOlderEvidence(t *testing.T) {
	t.Parallel()
	first := newChangeFixture(t, nil)
	for _, f := range []changeFixture{first, newChangeFixture(t, first.service, first.project.OrganizationID+"_second")} {
		version := f.publish(t, "version", "")
		for _, family := range []string{"check", "landing exit"} {
			t.Run(f.change.ID+"/"+family, func(t *testing.T) {
				if family == "landing exit" {
					actor := tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}
					appendTestHealthEventAs(t, f.nativeFixture, actor, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Merging", ToState: "Rework", Reason: "landing_refused"}, f.service.config.Clock())
					for range hubrecords.ChangeLegacyReadLimit - 1 {
						appendTestHealthEventAs(t, f.nativeFixture, actor, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, f.service.config.Clock())
					}
					read := func(message string) {
						t.Helper()
						value, err := f.service.readNativeRecoveryTenant(t.Context(), analyticsTestScope(t, f.nativeFixture), string(f.issue.WorkItemID))
						if err != nil || len(value.History) != 1 || value.History[0].Data.FromState != "Merging" || value.History[0].Data.Reason != "landing_refused" {
							t.Fatalf("%s: history=%+v error=%v", message, value.History, err)
						}
					}
					read("boundary hid older evidence")
					appendTestHealthEventAs(t, f.nativeFixture, actor, f.issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, f.service.config.Clock())
					read("legacy overflow hid older evidence")
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.EventsBefore(t.Context(), f.project.ID, f.issue.WorkItemID, math.MaxInt64, corestore.MaxPageSize)
						if err != nil {
							return err
						}
						for _, row := range rows {
							if row.Type == "workflow.transitioned" && row.FromState == "" {
								t.Fatal("canonical workflow event lost its typed from-state projection")
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					read("converted history hid older evidence")
					return
				}
				fixture, target, versions := historyRepairSource(t, f, version, 1)
				version := versions[0]
				for index := range hubrecords.ChangeLegacyReadLimit + 1 {
					id, conclusion := fmt.Sprintf("other-%03d", index), ""
					if index == 0 {
						id, conclusion = "wanted", "failure"
					}
					check := tracker.ChangeCheck{VersionID: version.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: id, Conclusion: conclusion}}
					fixture.Add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.change.ID, "version_id": version.ID, "kind": "check", "source_key": id, "record_json": string(historyTestJSON(t, check))})
				}
				source := historyRepairImportSource(t, target, fixture)
				interrupted := errors.New("historical evidence import interrupted")
				pause := &historyRepairPauseDetails{DetailStore: source.Objects, marker: []byte(`"check_run_id":"other-100"`), failure: interrupted}
				source.Objects = pause
				if _, err := coreimport.ImportCoreChanges(t.Context(), target.service.config.TenantRepository.(*postgres.Repository), source); !errors.Is(err, interrupted) || !pause.fired {
					t.Fatalf("historical import did not pause after the boundary: %v", err)
				}
				read := func(through int64, message string) {
					t.Helper()
					var rows []eventstore.Evidence
					if err := target.service.config.TenantRepository.ReadTenant(t.Context(), target.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						reader, ok := r.(eventstore.ChangeRecoveryReader)
						if !ok {
							return hubapi.NativeRunEventStorageUnavailable()
						}
						var err error
						rows, _, err = reader.RecoveryChangeEvidence(t.Context(), target.change.ID, version.ID, through, []string{"wanted"})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 {
						t.Fatalf("%s: selected=%d", message, len(rows))
					}
					raw, err := eventstore.NewContent(target.service.runDetailStore()).Evidence(t.Context(), rows[0])
					if err != nil {
						t.Fatal(err)
					}
					var check tracker.ChangeCheck
					if json.Unmarshal(raw, &check) != nil || check.CheckRunID != "wanted" || check.Conclusion != "failure" {
						t.Fatalf("%s: restored=%+v", message, check)
					}
				}
				source.Objects = source.SourceObjects
				read(hubrecords.ChangeLegacyReadLimit, "boundary hid older evidence")
				historyRepairImport(t, target, source, 1)
				read(hubrecords.ChangeLegacyReadLimit+1, "legacy overflow hid older evidence")
				historyRepairImport(t, target, source, 1)
				read(hubrecords.ChangeLegacyReadLimit+1, "converted history hid older evidence")
			})
		}
	}
}

func TestLegacyClaimReviewsRetainGateAuthority(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "initial", "")
	fixture, target, versions := historyRepairSource(t, f, version, 1)
	version = versions[0]
	reviews := []tracker.ChangeReview{
		{ID: "human-approval", Actor: tracker.Actor{PrincipalID: "first-reviewer"}, Decision: "approved"},
		{ID: "old-implementation", ImplementationReview: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		{ID: "old-validator", Validator: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		{ID: "source-rejection", SecurityAudit: &tracker.SecurityAuditReview{}, Decision: "changes_requested"},
		{ID: "landing-rejection", SecurityAudit: &tracker.SecurityAuditReview{Landing: true}, Decision: "changes_requested"},
	}
	for i := range hubcontent.ClaimEvidenceLimit + 1 {
		reviews = append(reviews, tracker.ChangeReview{ID: fmt.Sprintf("comment-%d", i), Actor: tracker.Actor{PrincipalID: "first-reviewer"}, Decision: "commented"})
	}
	reviews = append(reviews,
		tracker.ChangeReview{ID: "human-refusal", Actor: tracker.Actor{PrincipalID: "second-reviewer"}, Decision: "changes_requested"},
		tracker.ChangeReview{ID: "implementation", ImplementationReview: &gate.ValidatorResult{Verdict: "fail"}, Decision: "changes_requested"},
		tracker.ChangeReview{ID: "validator", Validator: &gate.ValidatorResult{Verdict: "pass"}, Decision: "approved"},
		tracker.ChangeReview{ID: "source-approval", SecurityAudit: &tracker.SecurityAuditReview{}, Decision: "approved"},
		tracker.ChangeReview{ID: "landing-approval", SecurityAudit: &tracker.SecurityAuditReview{Landing: true}, Decision: "approved"},
	)
	for index, review := range reviews {
		review.VersionID = version.ID
		fixture.Add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.change.ID, "version_id": version.ID, "kind": "review", "source_key": nil, "record_json": string(historyTestJSON(t, review)), "payload_object_key": ""})
	}
	source := historyRepairImportSource(t, target, fixture)
	for range 2 {
		historyRepairImport(t, target, source, 1)
		var rows []eventstore.Evidence
		if err := target.service.config.TenantRepository.ReadTenant(t.Context(), target.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(tenantstore.ClaimChangeRowsReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			facts, err := reader.ClaimChangeRows(t.Context(), target.project.ID, []string{target.change.ID})
			rows = facts[target.change.ID].Evidence
			return err
		}); err != nil {
			t.Fatal(err)
		}
		selected := make([]tracker.ChangeReview, 0, len(rows))
		for _, row := range rows {
			if row.Kind != "review" {
				continue
			}
			raw, err := eventstore.NewContent(target.service.runDetailStore()).Evidence(t.Context(), row)
			if err != nil {
				t.Fatal(err)
			}
			var review tracker.ChangeReview
			if err := json.Unmarshal(raw, &review); err != nil {
				t.Fatal(err)
			}
			selected = append(selected, review)
		}
		ids := make([]string, 0, len(selected))
		for _, review := range selected {
			ids = append(ids, review.ID)
		}
		want := []string{"human-approval", "source-rejection", "landing-rejection", "human-refusal", "implementation", "validator", "source-approval", "landing-approval"}
		if !slices.Equal(ids, want) {
			t.Fatalf("review selection lost gate authority: got %v want %v", ids, want)
		}
	}
}

func historyRepairSource(t *testing.T, f changeFixture, base tracker.ChangeVersion, count int) (*neutralimporttest.Fixture, changeFixture, []tracker.ChangeVersion) {
	t.Helper()
	change := f.change
	change.ID, change.Revision, change.Landed = hubapi.NewID("change"), 1, nil
	versions := make([]tracker.ChangeVersion, count)
	for index := range versions {
		versions[index] = base
		versions[index].ID, versions[index].ChangeID, versions[index].Number = fmt.Sprintf("%s_version_%04d", change.ID, index), change.ID, int64(index+1)
	}
	change.CurrentVersion = versions[len(versions)-1].ID
	fixture := newCoreNeutralFixture(t, f.project.OrganizationID, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.Add("policy_revisions", map[string]any{"scope": string(f.project.OrganizationID) + "/" + string(f.project.ID), "policy_id": base.PolicyID, "metadata_json": string(historyTestJSON(t, base.Policy))})
	fixture.Add("change_requests", map[string]any{"id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": f.issue.WorkItemID, "record_json": string(historyTestJSON(t, change))})
	for _, item := range change.LinkedIssues {
		fixture.Add("change_issue_links", map[string]any{"change_id": change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": item})
	}
	var bundle []byte
	if base.Source != nil {
		row, err := f.service.readChangeVersionRecord(t.Context(), f.project.OrganizationID, f.change.ID, base.ID)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err = eventstore.NewContent(f.service.runDetailStore()).SourceBundle(t.Context(), f.project.OrganizationID, row, *base.Source)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range versions {
		fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": change.ID, "number": version.Number, "record_json": string(historyTestJSON(t, version)), "policy_revision_id": version.PolicyID, "repository": version.Repository})
		if version.Source != nil {
			fixture.Add("change_sources", map[string]any{"version_id": version.ID, "bundle": bundle, "object_key": "", "object_sha256": "", "object_bytes": 0, "content_bytes": 0})
		}
	}
	f.change, f.path = change, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/changes/"+change.ID
	return fixture, f, versions
}

func historyRepairImportSource(t *testing.T, target changeFixture, fixture *neutralimporttest.Fixture) coreimport.CoreChangeImportSource {
	t.Helper()
	objects := target.service.runDetailStore()
	return coreimport.CoreChangeImportSource{BundlePath: fixture.Write(), OrganizationID: target.project.OrganizationID, SourceObjects: objects, Objects: objects}
}

func historyRepairImport(t *testing.T, target changeFixture, source coreimport.CoreChangeImportSource, count int64) {
	t.Helper()
	got, err := coreimport.ImportCoreChanges(t.Context(), target.service.config.TenantRepository.(*postgres.Repository), source)
	if err != nil || got != count {
		t.Fatalf("historical repair import count=%d want=%d error=%v", got, count, err)
	}
}

type historyRepairPauseDetails struct {
	attachment.DetailStore
	marker  []byte
	failure error
	fired   bool
}

func (s *historyRepairPauseDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if !s.fired && bytes.Contains(raw, s.marker) {
		s.fired = true
		return attachment.ArchiveReference{}, s.failure
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}
