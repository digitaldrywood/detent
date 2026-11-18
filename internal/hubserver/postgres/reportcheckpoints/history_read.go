package reportcheckpoints

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HistoryPosition struct{ Through, Head int64 }

type historyKeys struct {
	projects, items []string
	first, last     []int64
}

func (k *historyKeys) add(item Item, first, last int64) {
	if last < first {
		return
	}
	k.projects = append(k.projects, item.Project)
	k.items = append(k.items, item.WorkItem)
	k.first = append(k.first, first)
	k.last = append(k.last, last)
}

func (k *historyKeys) plan(item Item, issue *reportstore.HistoryIssue, blocks []historyBlock, from time.Time, pos HistoryPosition) {
	watermark := issue.CreatedAt
	prefix := true
	for _, block := range blocks {
		if block.count == 0 {
			continue
		}
		old := block.max.Before(from)
		switch {
		case prefix && old && block.base.Last.Before(from) && block.base.LaneSeconds != nil:
			base := block.base
			issue.Base = &base
		case old && block.max.Before(watermark):
			prefix = false
			issue.OmittedPartial = true
		default:
			prefix = false
			k.add(item, block.through-FeedBatch+1, block.through)
			if block.min.Before(from) && block.min.After(watermark) {
				watermark = *block.min
			}
			if old && block.max.After(watermark) {
				watermark = *block.max
			}
			continue
		}
		issue.Omitted += block.count
		if block.max.After(watermark) {
			watermark = *block.max
		}
		issue.OmittedUnclassified = issue.OmittedUnclassified || block.unclassified
		for n, id := range block.reopenIDs {
			issue.Earlier = append(issue.Earlier, reportstore.WorkflowEvent{ID: id, VersionID: block.reopenVersions[n], At: block.reopenAt[n], Type: "workflow.transitioned", FromState: block.reopenFrom[n], ToState: block.reopenTo[n]})
		}
	}
	k.add(item, pos.Through+1, pos.Head)
}

func readHistoryBlocks(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, query reportstore.OverviewQuery, projects []reportstore.ProjectHistory, heads map[string]HistoryPosition, limit int) ([]string, int, error) {
	var raw, ids, projectIDs []string
	var ends []int64
	const blockLimit = 400
	remaining := int64(blockLimit)
	selected := map[string]bool{}
	large := map[string]bool{}
	var keys historyKeys
	for _, p := range projects {
		for _, i := range p.Issues {
			h := heads[string(i.WorkItemID)]
			if h.Through > 0 && h.Head-h.Through < FeedBatch && h.Through/FeedBatch <= remaining {
				remaining -= h.Through / FeedBatch
				selected[string(i.WorkItemID)] = true
				projectIDs = append(projectIDs, string(p.ProjectID))
				ends = append(ends, h.Through)
				ids = append(ids, string(i.WorkItemID))
			} else if h.Through > 0 && h.Head-h.Through < FeedBatch {
				large[string(i.WorkItemID)] = true
			} else {
				raw = append(raw, string(i.WorkItemID))
			}
		}
	}
	if len(ids) == 0 && len(large) == 0 {
		return raw, 0, nil
	}
	byItem, err := readBlocks(ctx, db, organization, projectIDs, ids, ends)
	if err != nil {
		return nil, 0, err
	}
	issues := map[Item]*reportstore.HistoryIssue{}
	count := 0
	largeRemaining := limit
	for _, p := range projects {
		key := historyStateKey(p.States)
		for n := range p.Issues {
			i := &p.Issues[n]
			id := string(i.WorkItemID)
			head := heads[id]
			item := Item{string(p.ProjectID), id}
			blocks := byItem[id]
			if large[id] {
				var lower int
				blocks, lower, err = readHistoricalBlocks(ctx, db, organization, item, query.From, query.To, head, largeRemaining)
				if err != nil {
					return nil, 0, err
				}
				if lower < 0 {
					raw = append(raw, id)
					continue
				}
				largeRemaining -= lower
			} else if !selected[id] {
				continue
			} else if len(blocks) == 0 || blocks[len(blocks)-1].through != head.Through {
				raw = append(raw, id)
				continue
			}
			if !validHistoryBlocks(blocks, i, key) {
				raw = append(raw, id)
				continue
			}
			start := len(keys.items)
			scratch := *i
			keys.plan(item, &scratch, blocks, query.From, head)
			if len(keys.items) > 800 {
				keys.items, keys.projects = keys.items[:start], keys.projects[:start]
				keys.first, keys.last = keys.first[:start], keys.last[:start]
				raw = append(raw, id)
				continue
			}
			*i = scratch
			issues[item] = i
			count += i.Omitted
		}
	}
	if count > limit {
		return nil, 0, reportstore.ErrPopulationExceeded
	}
	if len(keys.items) == 0 {
		return raw, count, nil
	}
	count, err = keys.readRanges(ctx, db, organization, query.To, issues, count, limit)
	return raw, count, err
}

func (k *historyKeys) readRanges(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, to time.Time, issues map[Item]*reportstore.HistoryIssue, count, limit int) (int, error) {
	rows, err := db.Query(ctx, `WITH ranges AS MATERIALIZED (
 SELECT * FROM unnest($2::text[],$3::text[],$4::bigint[],$5::bigint[]) k(project,item,first,last) LIMIT 800)
 SELECT k.project,k.item,e.id,e.version_id,e.recorded_at,e.type,e.from_state,e.to_state,coalesce(f.rework_class,''),coalesce(f.rework_kind,'')
 FROM ranges k CROSS JOIN LATERAL (SELECT e.sequence,e.id,e.version_id,e.recorded_at,e.type,e.from_state,e.to_state FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=k.project AND e.work_item_id=k.item AND e.sequence>=k.first AND e.sequence<=k.last AND e.type IN ('workflow.transitioned','scheduler.decision') ORDER BY e.sequence LIMIT 256) e
 LEFT JOIN LATERAL (SELECT f.rework_class,f.rework_kind FROM report_event_facts f WHERE f.organization_id=$1 AND f.event_id=e.id LIMIT 1) f ON true
 WHERE e.recorded_at<$6 ORDER BY k.project,k.item,e.sequence`, organization, k.projects, k.items, k.first, k.last, to)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item Item
		var e reportstore.WorkflowEvent
		if err := rows.Scan(&item.Project, &item.WorkItem, &e.ID, &e.VersionID, &e.At, &e.Type, &e.FromState, &e.ToState, &e.Class.Name, &e.Class.Kind); err != nil {
			return 0, err
		}
		i := issues[item]
		i.Events = append(i.Events, e)
		count++
	}
	if count > limit {
		return 0, reportstore.ErrPopulationExceeded
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read checkpoint history events: %w", err)
	}
	return count, nil
}

func validHistoryBlocks(blocks []historyBlock, issue *reportstore.HistoryIssue, key string) bool {
	for _, b := range blocks {
		if b.key != key || b.initial != issue.InitialState || !b.created.Equal(issue.CreatedAt) {
			return false
		}
	}
	return true
}

func readBlocks(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, projectIDs, ids []string, ends []int64) (map[string][]historyBlock, error) {
	byItem := map[string][]historyBlock{}
	if len(ids) == 0 {
		return byItem, nil
	}
	rows, err := db.Query(ctx, `WITH positions AS MATERIALIZED (
 SELECT k.project,k.item,b.seq FROM unnest($2::text[],$3::text[],$4::bigint[]) k(project,item,last)
 CROSS JOIN LATERAL generate_series(256::bigint,k.last,256::bigint) b(seq) LIMIT 400)
 SELECT pos.item,`+blockColumns+` FROM positions pos CROSS JOIN LATERAL (SELECT * FROM report_history_checkpoints h
 WHERE h.organization_id=$1 AND h.project_id=pos.project AND h.work_item_id=pos.item AND h.through_sequence=pos.seq LIMIT 1) h
 ORDER BY pos.project,pos.item,pos.seq`, organization, projectIDs, ids, ends)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var b historyBlock
		if err := b.scan(rows, &id); err != nil {
			rows.Close()
			return nil, err
		}
		byItem[id] = append(byItem[id], b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return byItem, nil
}
