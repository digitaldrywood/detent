package boardprojection_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/policy"
)

func TestBoardCardsRecordEvaluatedChangeProvenance(t *testing.T) {
	t.Parallel()
	h := quietHub(t)
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
	h := quietHub(t)
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

func TestLegacyPolicyRevisionsGainCompatibilityFactsThroughMaintenance(t *testing.T) {
	t.Parallel()
	h := quietHub(t)
	legacy := hubfixture.NewChangeInOrganization(t, h)
	version := legacy.Publish(t, "legacy-facts", "")
	untouched := hubfixture.NewChangeInOrganization(t, h)
	untouched.Publish(t, "current-facts", "")
	h.RefreshBoard(t, legacy.Organization())
	h.RefreshBoard(t, untouched.Organization())
	facts := func(t *testing.T, c hubfixture.Change) []string {
		t.Helper()
		rows, err := h.Admin.QueryContext(t.Context(), `SELECT 'policy '||policy_id||' '||payload_sha256||' '||gate_github_pull_request||' '||gate_kind||' '||gate_automated_review||' '||gate_required_checks||' '||gate_validator||' '||gate_security_audit FROM policy_revision_compatibility WHERE organization_id=$1 AND project_id=$2
 UNION ALL SELECT 'review '||review_policy_id||' '||policy_id||' '||payload_sha256||' '||require_review||' '||required_check_count FROM review_policy_revision_compatibility WHERE organization_id=$1 AND project_id=$2
 UNION ALL SELECT 'check '||review_policy_id||' '||ordinal||' '||name FROM review_policy_revision_checks WHERE organization_id=$1 AND project_id=$2 ORDER BY 1`, c.Organization(), c.Project.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			result = append(result, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	recorded, other := facts(t, legacy), facts(t, untouched)
	if len(recorded) < 2 || len(other) < 2 {
		t.Fatalf("recording owners wrote facts %v and %v, want policy and review facts for each project", recorded, other)
	}
	tx, err := h.Admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"SET LOCAL session_replication_role=replica", "DELETE FROM review_policy_revision_checks WHERE organization_id=$1", "DELETE FROM review_policy_revision_compatibility WHERE organization_id=$1", "DELETE FROM policy_revision_compatibility WHERE organization_id=$1"} {
		args := []any{legacy.Organization()}
		if statement == "SET LOCAL session_replication_role=replica" {
			args = nil
		}
		if _, err := tx.ExecContext(t.Context(), statement, args...); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var referenced []string
	for _, row := range recorded {
		if strings.Contains(row, " "+version.PolicyID+" ") && strings.HasPrefix(row, "policy ") || strings.Contains(row, " "+version.ReviewPolicy.ID+" ") {
			referenced = append(referenced, row)
		}
	}
	revisions := 0
	for _, row := range referenced {
		if !strings.HasPrefix(row, "check ") {
			revisions++
		}
	}
	if revisions != 2 {
		t.Fatalf("referenced revisions %v, want the version policy and review policy", referenced)
	}
	for step := 1; step <= revisions+1; step++ {
		h.RefreshBoard(t, legacy.Organization())
		restored := 0
		for _, row := range facts(t, legacy) {
			if !strings.HasPrefix(row, "check ") {
				restored++
			}
		}
		if restored != min(step, revisions) {
			t.Fatalf("maintenance pass %d restored %d revisions, want one revision per pass up to %d", step, restored, revisions)
		}
	}
	if got := facts(t, legacy); strings.Join(got, "\n") != strings.Join(referenced, "\n") {
		t.Fatalf("backfilled facts %v, want the recorded facts of the revisions the card and project reference %v", got, referenced)
	}
	if got := facts(t, untouched); strings.Join(got, "\n") != strings.Join(other, "\n") {
		t.Fatalf("another tenant's facts changed: %v, want %v", got, other)
	}
}
