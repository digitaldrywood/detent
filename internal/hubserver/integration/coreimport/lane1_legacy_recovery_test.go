package coreimport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
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
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLegacyChangeRecovery(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	t.Run("older check survives interrupted import", func(t *testing.T) {
		testLegacyCheckRecovery(t, hubfixture.NewNamedChange(t, h, "Legacy check", "org_legacy_check"))
	})
	t.Run("older landing exit survives bounded recovery", func(t *testing.T) {
		testLegacyLandingExit(t, hubfixture.NewNamedChange(t, h, "Legacy exit", "org_legacy_exit"))
	})
	t.Run("claim keeps effective review decisions", func(t *testing.T) {
		testLegacyClaimReviewAuthority(t, hubfixture.NewNamedChange(t, h, "Legacy claim reviews", "org_legacy_claim"))
	})
}

func testLegacyCheckRecovery(t *testing.T, f hubfixture.Change) {
	t.Helper()
	base := f.Publish(t, "version", "")
	fixture, target, versions := historyRepairSource(t, f, base, 1)
	version := versions[0]
	for index := range hubrecords.ChangeLegacyReadLimit + 1 {
		id, conclusion := fmt.Sprintf("other-%03d", index), ""
		if index == 0 {
			id, conclusion = "wanted", "failure"
		}
		check := tracker.ChangeCheck{VersionID: version.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: id, Conclusion: conclusion}}
		fixture.Add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.Change.ID, "version_id": version.ID, "kind": "check", "source_key": id, "record_json": string(historyTestJSON(t, check))})
	}
	source := historyRepairImportSource(t, target, fixture)
	interrupted := errors.New("historical evidence import interrupted")
	marker := fmt.Sprintf(`"check_run_id":"other-%03d"`, hubrecords.ChangeLegacyReadLimit)
	pause := &historyRepairPauseDetails{DetailStore: source.Objects, marker: []byte(marker), failure: interrupted}
	source.Objects = pause
	if _, err := coreimport.ImportCoreChanges(t.Context(), target.Hub.Repository, source); !errors.Is(err, interrupted) || !pause.fired {
		t.Fatalf("historical import did not pause at the last check: %v", err)
	}
	read := func(through int64, message string) {
		t.Helper()
		var rows []eventstore.Evidence
		if err := target.Hub.Repository.ReadTenant(t.Context(), target.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(eventstore.ChangeRecoveryReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			var err error
			rows, _, err = reader.RecoveryChangeEvidence(t.Context(), target.Change.ID, version.ID, through, []string{"wanted"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: expected only the matching check, got %+v", message, rows)
		}
		raw, err := eventstore.NewContent(target.Hub.Config.RunDetails).Evidence(t.Context(), rows[0])
		if err != nil {
			t.Fatal(err)
		}
		var check tracker.ChangeCheck
		if err := json.Unmarshal(raw, &check); err != nil || check.CheckRunID != "wanted" || check.Conclusion != "failure" {
			t.Fatalf("%s: restored=%+v error=%v", message, check, err)
		}
	}
	source.Objects = source.SourceObjects
	read(hubrecords.ChangeLegacyReadLimit, "interrupted import hid older evidence")
	historyRepairImport(t, target, source, 1)
	read(hubrecords.ChangeLegacyReadLimit+1, "legacy overflow hid older evidence")
	historyRepairImport(t, target, source, 1)
	read(hubrecords.ChangeLegacyReadLimit+1, "reimport hid older evidence")
}

func testLegacyLandingExit(t *testing.T, f hubfixture.Change) {
	t.Helper()
	f.Publish(t, "version", "")
	appendLegacyTransition(t, f, tracker.CollaborationData{FromState: "Merging", ToState: "Rework", Reason: "landing_refused"})
	unrelated := tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}
	for range hubrecords.ChangeLegacyReadLimit - 1 {
		appendLegacyTransition(t, f, unrelated)
	}
	read := func(message string) {
		t.Helper()
		path := f.Base + "/work-items/" + string(f.Issue.WorkItemID) + "?view=recovery&projection=bounded"
		response := f.Hub.Request(t, http.MethodGet, path, f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var value tracker.NativeRecovery
		hubfixture.Decode(t, response, &value)
		if len(value.History) != 1 || value.History[0].Data.FromState != "Merging" || value.History[0].Data.Reason != "landing_refused" {
			t.Fatalf("%s: history=%+v", message, value.History)
		}
	}
	read("boundary hid older evidence")
	appendLegacyTransition(t, f, unrelated)
	read("legacy overflow hid older evidence")
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.EventsBefore(t.Context(), f.Project.ID, f.Issue.WorkItemID, math.MaxInt64, corestore.MaxPageSize)
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
}

func appendLegacyTransition(t *testing.T, f hubfixture.Change, data tracker.CollaborationData) {
	t.Helper()
	value := tracker.CollaborationEvent{
		ID: hubapi.NewID("evt"), OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, AggregateType: "issue", AggregateID: f.Issue.WorkItemID,
		Type: "workflow.transitioned", SchemaVersion: 1, RecordedAt: f.Hub.Now(), Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, Data: data,
	}
	event, err := eventstore.NewContent(f.Hub.Config.RunDetails).PrepareEvent(t.Context(), f.Project.OrganizationID, historyTestJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyChange(t, f, func(w tenantstore.Writer) error {
		_, err := w.AppendEvent(t.Context(), event)
		return err
	})
}

func testLegacyClaimReviewAuthority(t *testing.T, f hubfixture.Change) {
	t.Helper()
	version := f.Publish(t, "initial", "")
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
		fixture.Add("change_evidence", map[string]any{"sequence": index + 1, "change_id": target.Change.ID, "version_id": version.ID, "kind": "review", "source_key": nil, "record_json": string(historyTestJSON(t, review)), "payload_object_key": ""})
	}
	source := historyRepairImportSource(t, target, fixture)
	for range 2 {
		historyRepairImport(t, target, source, 1)
		var rows []eventstore.Evidence
		if err := target.Hub.Repository.ReadTenant(t.Context(), target.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(tenantstore.ClaimChangeRowsReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			facts, err := reader.ClaimChangeRows(t.Context(), target.Project.ID, []string{target.Change.ID})
			rows = facts[target.Change.ID].Evidence
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			if row.Kind != "review" {
				continue
			}
			raw, err := eventstore.NewContent(target.Hub.Config.RunDetails).Evidence(t.Context(), row)
			if err != nil {
				t.Fatal(err)
			}
			var review tracker.ChangeReview
			if err := json.Unmarshal(raw, &review); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, review.ID)
		}
		want := []string{"human-approval", "source-rejection", "landing-rejection", "human-refusal", "implementation", "validator", "source-approval", "landing-approval"}
		if !slices.Equal(ids, want) {
			t.Fatalf("review selection lost gate authority: got %v want %v", ids, want)
		}
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
