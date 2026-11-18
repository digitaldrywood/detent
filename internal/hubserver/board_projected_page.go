package hubserver

import (
	"context"
	"maps"

	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) prepareProjectedBoardPage(ctx context.Context, scope hubscope.NativeScope, records []viewstore.BoardRecord, exact bool, prepared map[tracker.NativeWorkItemID]boardPreparedRecord) *boardRecordPreparation {
	cards := make(map[tracker.NativeWorkItemID]tracker.NativeBoardCard, len(records))
	issues := make([]*tracker.NativeIssue, 0, len(records))
	var fallback []viewstore.BoardRecord
	for _, row := range records {
		if prior, ok := prepared[row.WorkItemID]; ok {
			cards[row.WorkItemID] = prior.card
			continue
		}
		if !row.Display.Ready || !row.Display.Exact {
			fallback = append(fallback, row)
			continue
		}
		card := boardview.ProjectedCard(row)
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: row.Display.ChangePrincipalID}) {
			card.Change = nil
		}
		cards[row.WorkItemID] = card
		issues = append(issues, &card.Issue)
	}
	finish := timeSpan(ctx, "board_relationships")
	err := s.hydrateWorkItemRelationshipFactsPage(ctx, scope, issues)
	finish()
	for _, issue := range issues {
		card := cards[issue.WorkItemID]
		card.Issue = *issue
		cards[issue.WorkItemID] = card
	}
	for _, row := range records {
		if _, ok := cards[row.WorkItemID]; !ok {
			continue
		}
		prepared[row.WorkItemID] = boardPreparedRecord{record: row, card: cards[row.WorkItemID], err: err}
	}
	if len(fallback) > 0 {
		old := s.prepareBoardPage(ctx, scope, fallback, s.config.Clock(), exact, prepared)
		for index, row := range fallback {
			card, err := old.card(ctx, index)
			prepared[row.WorkItemID] = boardPreparedRecord{record: row, card: card, err: err}
		}
		old.close()
	}
	prepared = maps.Clone(prepared)
	return prepareBoardRecordPage(ctx, records, func(_ context.Context, row viewstore.BoardRecord) (tracker.NativeBoardCard, error) {
		value := prepared[row.WorkItemID]
		return value.card, value.err
	})
}
