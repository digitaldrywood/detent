package changestaleness_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type objectReads struct {
	attachment.DetailStore
	reads atomic.Int64
}

func (d *objectReads) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	d.reads.Add(1)
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}

func quietHub(t *testing.T, details attachment.DetailStore) *hubfixture.Hub {
	t.Helper()
	if details == nil {
		details = hubtest.NewDetails()
	}
	cfg := hubserver.WithDeps(hubfixture.Config{RunDetails: details}, hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	return hubfixture.Open(t, cfg)
}

func boardStatuses(t *testing.T, p hubfixture.Native) map[tracker.NativeWorkItemID]string {
	t.Helper()
	response := p.Request(t, http.MethodGet, p.Base+"/work-items?include=board&limit=50", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var page tracker.NativeIssuePage
	hubfixture.Decode(t, response, &page)
	statuses := map[tracker.NativeWorkItemID]string{}
	for _, card := range page.Cards {
		if card.Change != nil {
			statuses[card.Issue.WorkItemID] = card.Change.Status
		}
	}
	return statuses
}

func approveDescriptor(t *testing.T, p hubfixture.Native, expected string, descriptor policy.Descriptor) {
	t.Helper()
	hubfixture.RequireStatus(t, p.Hub.Request(t, http.MethodPut, p.Base+"/policy", p.Hub.AdminToken(), policy.Change{ExpectedID: expected, Policy: descriptor}), http.StatusOK)
}

func instructionVariant(index int) policy.Descriptor {
	return hubtest.PolicyDescriptor(hubtest.WithPolicyPrompt(fmt.Sprintf("Instructions %d.", index)))
}

func TestBoardOverlayWorkIgnoresUnrelatedPolicyHistory(t *testing.T) {
	t.Parallel()
	details := &objectReads{DetailStore: hubtest.NewDetails()}
	h := quietHub(t, details)
	projects := map[string]hubfixture.Native{}
	for name, cards := range map[string]int{"small": 2, "large": 12} {
		p := hubfixture.NewNativeWith(t, h, "", hubtest.ProjectOptions{Name: "Example " + name + " " + hubapi.NewID("fixture")})
		first := hubfixture.NewChangeIn(t, p)
		first.Publish(t, name+"-0", "")
		for index := 1; index < cards; index++ {
			issue := p.Create(t, fmt.Sprintf("work %d", index))
			path := p.Base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response := p.Request(t, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change", Body: "Proposed work"})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			hubfixture.Decode(t, response, &change)
			c := hubfixture.Change{Native: p, Issue: issue, Change: change, Path: path + "/" + change.ID, Rules: first.Rules}
			c.Publish(t, fmt.Sprintf("%s-%d", name, index), "")
		}
		projects[name] = p
	}
	h.RefreshBoard(t, hubfixture.Primary)
	large := projects["large"]
	before := boardStatuses(t, large)
	if len(before) != 12 {
		t.Fatalf("large board statuses=%v, want 12 Change cards", before)
	}
	reads := details.reads.Load()
	boardStatuses(t, large)
	pageReads := details.reads.Load() - reads

	state := func(t *testing.T, p hubfixture.Native) (cards string, dirty, facts int) {
		t.Helper()
		if err := h.Admin.QueryRowContext(t.Context(), `SELECT coalesce(string_agg(work_item_id||':'||source_revision||':'||coalesce(card_change_base_status,'')||':'||updated_at, ',' ORDER BY work_item_id),''),
 (SELECT count(*) FROM native_board_dirty d WHERE d.organization_id=$1 AND d.project_id=$2),
 (SELECT count(*) FROM policy_revision_compatibility c WHERE c.organization_id=$1 AND c.project_id=$2)
 FROM native_board_items WHERE organization_id=$1 AND project_id=$2`, p.Organization(), p.Project.ID).Scan(&cards, &dirty, &facts); err != nil {
			t.Fatal(err)
		}
		return cards, dirty, facts
	}
	for name, p := range projects {
		expected := hubfixture.Policy().ID
		for index := range 20 {
			cards, dirty, facts := state(t, p)
			variant := instructionVariant(index)
			approveDescriptor(t, p, expected, variant)
			afterCards, afterDirty, afterFacts := state(t, p)
			if afterCards != cards || afterDirty != dirty || afterFacts != facts+1 {
				t.Fatalf("%s approval %d changed cards=%v dirty %d->%d facts %d->%d; want no card writes and one facts row", name, index, afterCards != cards, dirty, afterDirty, facts, afterFacts)
			}
			expected = variant.ID
		}
		approveDescriptor(t, p, expected, hubfixture.Policy())
	}

	reads = details.reads.Load()
	if got := boardStatuses(t, large); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("statuses after unrelated history %v, want %v", got, before)
	}
	if got := details.reads.Load() - reads; got != pageReads {
		t.Fatalf("page read %d objects after 20 unrelated revisions, want %d", got, pageReads)
	}
}

func TestBoardOverlayPendingUntilFactsAndImmediateStaleness(t *testing.T) {
	t.Parallel()
	h := quietHub(t, nil)
	c := hubfixture.NewChangeIn(t, hubfixture.NewNativeWith(t, h, "", hubtest.ProjectOptions{Name: "Example overlay " + hubapi.NewID("fixture")}))
	c.Publish(t, "overlay", "")
	h.RefreshBoard(t, c.Organization())
	status := func(t *testing.T) string {
		t.Helper()
		return boardStatuses(t, c.Native)[c.Issue.WorkItemID]
	}
	if got := status(t); got != "needs_evidence" {
		t.Fatalf("fixture status=%q, want needs_evidence", got)
	}
	tx, err := h.Admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"SET LOCAL session_replication_role=replica", "DELETE FROM review_policy_revision_checks WHERE organization_id=$1", "DELETE FROM review_policy_revision_compatibility WHERE organization_id=$1", "DELETE FROM policy_revision_compatibility WHERE organization_id=$1"} {
		var args []any
		if strings.Contains(statement, "$1") {
			args = append(args, c.Organization())
		}
		if _, err := tx.ExecContext(t.Context(), statement, args...); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := status(t); got != "pending" {
		t.Fatalf("status with incomplete policy facts=%q, want pending", got)
	}
	for range 2 {
		h.RefreshBoard(t, c.Organization())
	}
	if got := status(t); got != "needs_evidence" {
		t.Fatalf("status after maintenance backfilled facts=%q, want needs_evidence", got)
	}
	changed := hubtest.PolicyDescriptor(func(w *config.Workflow) { w.Config.Gate.SecurityAudit.Enabled = true })
	approveDescriptor(t, c.Native, hubfixture.Policy().ID, changed)
	if got := status(t); got != "stale_policy" {
		t.Fatalf("board status after an incompatible approval=%q, want stale_policy without maintenance", got)
	}
	response := c.Request(t, http.MethodGet, c.Path, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var detail tracker.ChangeDetail
	hubfixture.Decode(t, response, &detail)
	if detail.Summary.Status != "stale_policy" {
		t.Fatalf("change detail status=%q while the board shows stale_policy", detail.Summary.Status)
	}
	approveDescriptor(t, c.Native, changed.ID, hubfixture.Policy())
	if got := status(t); got != "needs_evidence" {
		t.Fatalf("board status after restoring the compatible policy=%q, want needs_evidence", got)
	}
}
