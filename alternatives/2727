package postgres

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
)

var boardListColumns = strings.ReplaceAll(boardPageColumns, "b.page_", "b.list_")

func boardListTargets(b *viewstore.BoardRecord) ([]any, func()) {
	var list viewstore.BoardRecord
	targets, finish := boardPageTargets(&list)
	return targets, func() { finish(); b.ListIssue = list.PageIssue }
}
