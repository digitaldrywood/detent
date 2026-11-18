package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardOverlapRepository struct {
	tenantstore.Repository
	tenantstore.SnapshotRepository
	beforePublish func()
}

func (r boardOverlapRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.beforePublish()
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

func TestBoardRefreshOverlappingReasonEdit(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	dependent, blocker := f.create(t, "Dependent"), f.create(t, "Blocker")
	edge := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "edge"}, ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", f.token, edge), http.StatusOK)
	repository := f.service.config.TenantRepository
	drain := func(t *testing.T) {
		t.Helper()
		for range boardMaintenanceBatch {
			_, processed, _, err := f.service.refreshNextBoardRecord(t.Context(), f.project.OrganizationID, true, "")
			if err != nil {
				t.Fatal(err)
			}
			if !processed {
				return
			}
		}
		t.Fatal("dirty board queue did not drain for two linked items")
	}
	publishDuring := func(t *testing.T, id tracker.NativeWorkItemID, edit func()) {
		t.Helper()
		publisher := &Service{config: f.service.config}
		publisher.config.TenantRepository = boardOverlapRepository{Repository: repository, SnapshotRepository: repository.(tenantstore.SnapshotRepository), beforePublish: edit}
		candidate := viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: id}
		err := publisher.refreshBoardRecord(t.Context(), f.project.OrganizationID, candidate, f.service.config.Clock())
		if err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatal(err)
		}
		drain(t)
	}
	for _, tc := range []struct {
		name, kind string
		id         tracker.NativeWorkItemID
		requeued   bool
		drained    bool
	}{
		{"requeued while dirty", "blocked", dependent.WorkItemID, true, false},
		{"queued while clean", "unblocks", blocker.WorkItemID, false, false},
		{"newer publication already drained", "blocked", dependent.WorkItemID, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := readWorkItem(t, f, tc.id, "")
			reason := issue.BlockedReason
			if tc.kind == "unblocks" {
				reason = issue.UnblocksReason
			}
			edit := func(text string) {
				request := tracker.UpdateDependencyReason{Mutation: tracker.Mutation{IdempotencyKey: tc.name + text}, WorkItemID: tc.id, Kind: tc.kind, Text: text, Basis: reason.CurrentBasis, ExpectedRevision: reason.Revision}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(tc.id)+"/dependency-reasons", f.token, request), http.StatusOK)
				reason.Revision++
			}
			edit("Earlier reason.")
			if !tc.requeued {
				drain(t)
			}
			publishDuring(t, tc.id, func() {
				edit("Updated reason.")
				if tc.drained {
					drain(t)
				}
			})
			err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				board, err := r.BoardItem(t.Context(), f.project.ID, tc.id)
				if err != nil {
					return err
				}
				if board.PageIssue == nil || board.ListIssue == nil {
					t.Fatal("drained board is missing its page or list projection")
				}
				for _, projected := range []*tracker.NativeIssue{board.PageIssue, board.ListIssue} {
					got := projected.BlockedReason
					if tc.kind == "unblocks" {
						got = projected.UnblocksReason
					}
					if got.Text != "Updated reason." || got.Revision != reason.Revision || projected.Revision != issue.Revision {
						t.Fatalf("after drain got reason %q revision %d, issue revision %d; want updated reason revision %d, issue revision %d", got.Text, got.Revision, projected.Revision, reason.Revision, issue.Revision)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("neighbor requeues an already dirty item", func(t *testing.T) {
		source := readWorkItem(t, f, dependent.WorkItemID, "")
		target := readWorkItem(t, f, blocker.WorkItemID, "")
		mark := tracker.UpdateDependencyReason{Mutation: tracker.Mutation{IdempotencyKey: "mark dependent"}, WorkItemID: source.WorkItemID, Kind: "blocked", Text: "Needs the blocker.", Basis: source.BlockedReason.CurrentBasis, ExpectedRevision: source.BlockedReason.Revision}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(source.WorkItemID)+"/dependency-reasons", f.token, mark), http.StatusOK)
		publishDuring(t, source.WorkItemID, func() {
			edit := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "rename blocker"}, ExpectedRevision: target.Revision, Title: new("Renamed blocker")}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(target.WorkItemID), f.token, edit), http.StatusOK)
			candidate := viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: target.WorkItemID}
			if err := f.service.refreshBoardRecord(t.Context(), f.project.OrganizationID, candidate, f.service.config.Clock()); err != nil {
				t.Fatal(err)
			}
		})
		err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			board, err := r.BoardItem(t.Context(), f.project.ID, source.WorkItemID)
			if err != nil {
				return err
			}
			if board.PageIssue == nil || len(board.PageIssue.Blockers) != 1 || board.PageIssue.Blockers[0].Title != "Renamed blocker" {
				t.Fatal("drained board lost the concurrent blocker rename")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestNativeDependencyReasonCAS(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	dependent, blocker := f.create(t, "Dependent"), f.create(t, "Blocker")
	edge := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "edge"}, ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", f.token, edge), http.StatusOK)
	current := readWorkItem(t, f, dependent.WorkItemID, "")
	neighbor := readWorkItem(t, f, blocker.WorkItemID, "")
	if len(current.Blockers) != 1 || current.Blockers[0].Title != "Blocker" || current.Blockers[0].Number != blocker.Number || len(neighbor.Dependents) != 1 || neighbor.Dependents[0].Title != "Dependent" {
		t.Fatalf("dependency summaries: %+v %+v", current.Blockers, neighbor.Dependents)
	}
	path := f.base + "/work-items/" + string(dependent.WorkItemID) + "/dependency-reasons"
	drainTestNativeBoard(t, f.service)
	boardSequence := func() (int64, error) {
		var sequence int64
		err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			head, err := r.(viewstore.BoardReader).BoardSequence(t.Context(), f.project.ID)
			sequence = head.Sequence
			return err
		})
		return sequence, err
	}
	beforeSequence, err := boardSequence()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, basis string
		revision    int64
		want        int
	}{
		{"stale basis", "outdated", 0, http.StatusConflict},
		{"stale revision", current.BlockedReason.CurrentBasis, 1, http.StatusConflict},
		{"current", current.BlockedReason.CurrentBasis, 0, http.StatusOK},
		{"second writer", current.BlockedReason.CurrentBasis, 0, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.UpdateDependencyReason{Mutation: tracker.Mutation{IdempotencyKey: test.name}, WorkItemID: dependent.WorkItemID, Kind: "blocked", Text: "Waits for the shared foundation.", Basis: test.basis, ExpectedRevision: test.revision}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request), test.want)
		})
	}
	current = readWorkItemWithoutBoardRefresh(t, f, dependent.WorkItemID, "")
	if current.BlockedReason.Text != "Waits for the shared foundation." || current.Revision != 2 || current.State != dependent.State {
		t.Fatalf("reason changed issue state or revision: %+v", current)
	}
	dirty := []string{}
	rows, err := usagePostgresAdmin(t, f.service).QueryContext(t.Context(), "SELECT work_item_id FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2 ORDER BY work_item_id", f.project.OrganizationID, f.project.ID)
	if err == nil {
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			dirty = append(dirty, id)
		}
		if err == nil {
			err = rows.Err()
		}
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil || !slices.Equal(dirty, []string{string(dependent.WorkItemID)}) {
		t.Fatalf("reason invalidated other items: %v err=%v", dirty, err)
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := range 2 {
		wg.Go(func() {
			request := tracker.UpdateDependencyReason{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("concurrent-%d", i)}, WorkItemID: blocker.WorkItemID, Kind: "unblocks", Text: "Enables the dependent feature.", Basis: neighbor.UnblocksReason.CurrentBasis, ExpectedRevision: 0}
			statuses <- performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request).Code
		})
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent writers: %v", counts)
	}
	if afterSequence, err := boardSequence(); err != nil || afterSequence != beforeSequence {
		t.Fatalf("reason write synchronously projected cards: before=%d after=%d err=%v", beforeSequence, afterSequence, err)
	}
	drainTestNativeBoard(t, f.service)
	edge.IdempotencyKey = "remove"
	edge.ExpectedRevision = current.Revision
	edge.Operation = "remove"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(dependent.WorkItemID)+"/dependencies", f.token, edge), http.StatusOK)
	current = readWorkItem(t, f, dependent.WorkItemID, "")
	neighbor = readWorkItem(t, f, blocker.WorkItemID, "")
	if current.BlockedReason.Text != "" || neighbor.UnblocksReason.Text != "" || len(neighbor.Dependents) != 0 || current.BlockedReason.Basis == current.BlockedReason.CurrentBasis {
		t.Fatalf("stale reasons visible after removal: %+v %+v", current.BlockedReason, neighbor.UnblocksReason)
	}
}

func TestDependencyReasonPlainText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		text string
		want bool
	}{
		{"Waits for the settings foundation.", true}, {"Provides the HTTP v1.2 handlers.", true}, {"Uses ftp://example.test/files.", false}, {strings.Repeat("界", 140), true}, {strings.Repeat("界", 141), false}, {"<script>alert(1)</script>", false}, {"[settings](https://example.test)", false}, {"**Settings**", false}, {"Two\nlines", false}, {"First sentence. Second sentence.", false}, {" trailing ", false},
	} {
		t.Run(fmt.Sprintf("%q", test.text), func(t *testing.T) {
			if got := validDependencyReason(test.text); got != test.want {
				t.Fatalf("valid=%v want %v", got, test.want)
			}
		})
	}
}

func TestNativeDependencyBatchReadCost(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	blocker := f.create(t, "Shared foundation")
	dependent := f.create(t, "Dependent feature")
	for start := 3; start <= 2000; start += corestore.MaxPageSize {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			for number := start; number < min(start+corestore.MaxPageSize, 2001); number++ {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID,
					NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_batch_%d", number)), Number: int64(number), Title: fmt.Sprintf("Dependent feature %d", number),
					WorkflowStateName: new(dependent.State), ActorKind: new(dependent.Actor.Kind), ActorPrincipalID: new(dependent.Actor.PrincipalID)}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for start := 2; start <= 2000; start += corestore.MaxPageSize {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			for number := start; number < min(start+corestore.MaxPageSize, 2001); number++ {
				id := dependent.WorkItemID
				if number > 2 {
					id = tracker.NativeWorkItemID(fmt.Sprintf("wi_batch_%d", number))
				}
				for _, relation := range []corestore.Relationship{
					{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: id, RelatedProjectID: f.project.ID, RelatedWorkItemID: blocker.WorkItemID, Type: "dependency"},
					{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: id, RelatedProjectID: f.project.ID, RelatedWorkItemID: blocker.WorkItemID, Type: "related"},
				} {
					if err := w.SetRelationship(t.Context(), relation, "add", f.service.config.Clock()); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := analyticsTestScope(t, f)
	connection := startupConnection(t, usagePostgresAdmin(t, f.service), "detent_runtime_v2", "")
	config, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	config.ConnConfig.Tracer = runtimeHistoryTrace{}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	original := f.service.config.TenantRepository
	defer func() {
		f.service.config.TenantRepository = original
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	}()
	f.service.config.TenantRepository = repository
	for _, size := range []int{2, 2000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			issues := make([]*tracker.NativeIssue, size)
			issues[0] = &tracker.NativeIssue{NativeReference: blocker.NativeReference}
			issues[1] = &tracker.NativeIssue{NativeReference: dependent.NativeReference}
			for index := 2; index < size; index++ {
				issues[index] = &tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_batch_%d", index+1)), ProjectID: f.project.ID}}
			}
			capture := &runtimeHistoryTraceCapture{}
			start := time.Now()
			if err := f.service.hydrateWorkItemRelationshipFactsPage(context.WithValue(t.Context(), runtimeHistoryTraceKey{}, capture), scope, issues); err != nil {
				t.Fatal(err)
			}
			capture.mu.Lock()
			statements := slices.Clone(capture.statements)
			capture.mu.Unlock()
			queries := 0
			for _, statement := range statements {
				if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(statement.query)), "SELECT") || strings.Contains(statement.query, "-- name: RelationshipBatch :many") {
					if !strings.Contains(statement.query, "set_config(") {
						queries++
					}
				}
			}
			t.Logf("hydrated %d issues in %s with %d queries", size, time.Since(start), queries)
			if len(issues[0].Dependents) != 1999 || !issues[0].DependencyReady || len(issues[0].Blockers) != 0 {
				t.Fatalf("blocker hydration: dependents=%d ready=%v", len(issues[0].Dependents), issues[0].DependencyReady)
			}
			for _, issue := range issues[1:] {
				if len(issue.Blockers) != 1 || issue.Blockers[0].ID != blocker.WorkItemID || issue.Blockers[0].Title != "Shared foundation" || issue.DependencyReady || len(issue.Related) != 1 || issue.Related[0].ID != blocker.WorkItemID {
					t.Fatalf("dependent hydration: %+v", issue)
				}
			}
			if queries != 2 {
				t.Fatalf("dependency query count scales with cards: %d", queries)
			}
		})
	}
}
