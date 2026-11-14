package boardprojection_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/policy"
)

func TestBoardCardsRecordEvaluatedChangeProvenance(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{RunDetails: hubtest.NewDetails()})
	c := hubfixture.NewChangeInOrganization(t, h)
	version := c.Publish(t, "provenance", "")
	card := func(t *testing.T) viewstore.BoardRecord {
		t.Helper()
		h.RefreshBoard(t, c.Organization())
		var row viewstore.BoardRecord
		if err := h.Repository.ReadTenant(t.Context(), c.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.BoardItem(t.Context(), c.Project.ID, c.Issue.WorkItemID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return row
	}
	current := card(t)
	want := viewstore.BoardChangeProvenance{VersionID: version.ID, PolicyID: version.PolicyID, ReviewPolicyID: version.ReviewPolicy.ID, BaseStatus: "needs_evidence"}
	if current.Display.Change == nil || current.Display.Change.Status != "needs_evidence" || current.Display.ChangeProvenance == nil || *current.Display.ChangeProvenance != want {
		t.Fatalf("card change=%+v provenance=%+v, want %+v", current.Display.Change, current.Display.ChangeProvenance, want)
	}
	changed := hubfixture.Policy()
	changed.Gates.SecurityAudit = true
	changed = changed.WithID()
	hubfixture.RequireStatus(t, h.Request(t, http.MethodPut, c.Base+"/policy", h.AdminToken(), policy.Change{ExpectedID: hubfixture.Policy().ID, Policy: changed}), http.StatusOK)
	stale := card(t)
	if stale.Display.Change == nil || stale.Display.Change.Status != "stale_policy" || stale.Display.ChangeProvenance == nil || *stale.Display.ChangeProvenance != want {
		t.Fatalf("after an incompatible policy the card change=%+v provenance=%+v, want stale status and unchanged provenance %+v", stale.Display.Change, stale.Display.ChangeProvenance, want)
	}
}

func TestLegacyCardsGainChangeProvenanceThroughMaintenance(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{RunDetails: hubtest.NewDetails()})
	c := hubfixture.NewChangeInOrganization(t, h)
	version := c.Publish(t, "legacy", "")
	h.RefreshBoard(t, c.Organization())
	if _, err := h.Admin.ExecContext(t.Context(), "UPDATE native_board_items SET card_change_version_id=NULL,card_change_policy_id=NULL,card_change_review_policy_id=NULL,card_change_base_status=NULL WHERE organization_id=$1 AND project_id=$2", c.Organization(), c.Project.ID); err != nil {
		t.Fatal(err)
	}
	read := func(t *testing.T) viewstore.BoardRecord {
		t.Helper()
		var row viewstore.BoardRecord
		if err := h.Repository.ReadTenant(t.Context(), c.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.BoardItem(t.Context(), c.Project.ID, c.Issue.WorkItemID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return row
	}
	if legacy := read(t); legacy.Display.Change == nil || legacy.Display.ChangeProvenance != nil {
		t.Fatalf("legacy fixture change=%+v provenance=%+v", legacy.Display.Change, legacy.Display.ChangeProvenance)
	}
	h.RefreshBoard(t, c.Organization())
	want := viewstore.BoardChangeProvenance{VersionID: version.ID, PolicyID: version.PolicyID, ReviewPolicyID: version.ReviewPolicy.ID, BaseStatus: "needs_evidence"}
	if backfilled := read(t); backfilled.Display.ChangeProvenance == nil || *backfilled.Display.ChangeProvenance != want {
		t.Fatalf("maintenance did not backfill provenance: %+v, want %+v", backfilled.Display.ChangeProvenance, want)
	}
}
