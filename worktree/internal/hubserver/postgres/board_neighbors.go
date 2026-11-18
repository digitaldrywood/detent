package postgres

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardNeighbor struct {
	project tracker.ProjectID
	item    tracker.NativeWorkItemID
}

func boardNeighbors(record *viewstore.BoardRecord) []boardNeighbor {
	if record == nil {
		return nil
	}
	issue := record.PageIssue
	if issue == nil {
		issue = record.ListIssue
	}
	if issue == nil {
		return nil
	}
	var neighbors []boardNeighbor
	for _, links := range [][]tracker.NativeDependency{issue.Blockers, issue.Dependents, issue.Related} {
		for _, link := range links {
			neighbors = append(neighbors, boardNeighbor{project: link.ProjectID, item: link.ID})
		}
	}
	return neighbors
}

func sameBoardNeighborView(a, b *viewstore.BoardRecord) bool {
	if a == nil || b == nil {
		return a == b
	}
	sameTime := func(x, y *time.Time) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Equal(*y)
	}
	return a.Display.Title == b.Display.Title && a.Display.Number == b.Display.Number && a.Position.State == b.Position.State &&
		a.Terminal == b.Terminal && a.Dispatchable == b.Dispatchable && a.Archived == b.Archived &&
		a.ActorPrincipalID == b.ActorPrincipalID && sameTime(a.ClosedAt, b.ClosedAt)
}

func changedBoardNeighbors(previous, next *viewstore.BoardRecord) []boardNeighbor {
	before, after := boardNeighbors(previous), boardNeighbors(next)
	var changed []boardNeighbor
	if !sameBoardNeighborView(previous, next) {
		changed = append(before, after...)
	} else {
		for _, neighbor := range before {
			if !slices.Contains(after, neighbor) {
				changed = append(changed, neighbor)
			}
		}
		for _, neighbor := range after {
			if !slices.Contains(before, neighbor) {
				changed = append(changed, neighbor)
			}
		}
	}
	slices.SortFunc(changed, func(a, b boardNeighbor) int {
		return cmp.Or(cmp.Compare(a.project, b.project), cmp.Compare(a.item, b.item))
	})
	return slices.Compact(changed)
}

func (s *session) queueBoardNeighbors(ctx context.Context, previous, next *viewstore.BoardRecord) error {
	neighbors := changedBoardNeighbors(previous, next)
	if len(neighbors) == 0 {
		return nil
	}
	projects, items := make([]string, len(neighbors)), make([]string, len(neighbors))
	for i, neighbor := range neighbors {
		projects[i], items[i] = string(neighbor.project), string(neighbor.item)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO native_board_dirty(organization_id,project_id,work_item_id)
 SELECT $1,n.project,n.item FROM unnest($2::text[],$3::text[]) n(project,item)
 WHERE EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=$1 AND i.project_id=n.project AND i.native_id=n.item)
 ON CONFLICT (organization_id,project_id,work_item_id) DO UPDATE SET queued_at=native_board_dirty.queued_at`, s.organization, projects, items)
	return mapError(ctx, err)
}

func boardRelationshipsOnly(previous, next *viewstore.BoardRecord) bool {
	if previous == nil || next == nil {
		return false
	}
	before, after := withoutBoardRelationships(*previous), withoutBoardRelationships(*next)
	return sameBoardContent(&before, &after)
}

func withoutBoardRelationships(record viewstore.BoardRecord) viewstore.BoardRecord {
	record.Payload = attachment.ArchiveReference{}
	record.ListBytes = 0
	for _, issue := range []**tracker.NativeIssue{&record.PageIssue, &record.ListIssue} {
		if *issue == nil {
			continue
		}
		copied := **issue
		copied.Dependencies, copied.Blockers, copied.Dependents, copied.Related = nil, nil, nil, nil
		copied.DependencyReady = false
		copied.BlockedReason, copied.UnblocksReason = tracker.DependencyReason{}, tracker.DependencyReason{}
		*issue = &copied
	}
	if record.ReporterRelations != nil {
		record.ReporterRelations = &viewstore.BoardReporterRelations{ReporterStatus: record.ReporterRelations.ReporterStatus}
	}
	return record
}
