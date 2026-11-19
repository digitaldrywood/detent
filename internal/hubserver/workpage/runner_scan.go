package workpage

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const RunnerCursorPrefix = "runner."

func RunnerEnumeration(params url.Values) bool {
	cursor := params.Get("cursor")
	if !params.Has("cursor") || cursor == RunnerCursorPrefix || cursor != "" && !strings.HasPrefix(cursor, RunnerCursorPrefix) {
		return false
	}
	for name := range params {
		switch name {
		case "cursor", "limit", "state", "archived":
		default:
			return false
		}
	}
	return params.Get("archived") == "all" && !params.Has("state") ||
		len(params["state"]) == 1 && params.Get("state") != "" && !params.Has("archived")
}

type RunnerScan struct {
	IDs       []tracker.NativeWorkItemID
	positions map[tracker.NativeWorkItemID]int64
	through   int64
	next      corestore.ScanCursor
}

func ReadRunnerScan(ctx context.Context, reader corestore.RunnerWorkItemReader, project tracker.ProjectID, state string, position *corestore.ScanCursor, limit int) (RunnerScan, error) {
	var after corestore.ScanCursor
	if position != nil {
		after = *position
	}
	page, err := reader.RunnerWorkItemKeys(ctx, project, state, after, limit)
	if err != nil {
		return RunnerScan{}, err
	}
	scan := RunnerScan{
		IDs:       make([]tracker.NativeWorkItemID, len(page.Items)),
		positions: make(map[tracker.NativeWorkItemID]int64, len(page.Items)),
		next:      page.Next,
	}
	for i, item := range page.Items {
		scan.IDs[i] = item.NativeID
		scan.positions[item.NativeID] = item.ID
		scan.through = item.ID
	}
	if page.Next != (corestore.ScanCursor{}) {
		scan.through = page.Next.Through
	}
	return scan, nil
}

func (s RunnerScan) Order(rows []viewstore.BoardRecord) {
	slices.SortFunc(rows, func(a, b viewstore.BoardRecord) int {
		return cmp.Compare(s.positions[a.WorkItemID], s.positions[b.WorkItemID])
	})
}

func (s RunnerScan) Cursors(cursor Cursor, key []byte, rows []viewstore.BoardRecord) (string, []string, error) {
	items := make([]string, 0, len(rows))
	for _, row := range rows {
		cursor.Scan = &corestore.ScanCursor{After: s.positions[row.WorkItemID], Through: s.through}
		encoded, err := Encode(cursor, key)
		if err != nil {
			return "", nil, err
		}
		items = append(items, RunnerCursorPrefix+encoded)
	}
	if s.next == (corestore.ScanCursor{}) {
		return "", items, nil
	}
	cursor.Scan = &s.next
	next, err := Encode(cursor, key)
	if err != nil {
		return "", nil, err
	}
	return RunnerCursorPrefix + next, items, nil
}
