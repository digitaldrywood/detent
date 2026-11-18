package postgres

import (
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
)

func sameBoardPageContent(a, b *viewstore.BoardRecord) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !sameBoardPageContentOnly(a, b) {
		return false
	}
	leftRecord, rightRecord := *a, *b
	leftRecord.PageIssue, rightRecord.PageIssue = a.ListIssue, b.ListIssue
	leftRecord.ReporterRelations, rightRecord.ReporterRelations = nil, nil
	return sameBoardPageContentOnly(&leftRecord, &rightRecord)
}

func sameBoardPageContentOnly(a, b *viewstore.BoardRecord) bool {
	left := newBoardPageModel(a.PageIssue, a.ReporterRelations).values()
	right := newBoardPageModel(b.PageIssue, b.ReporterRelations).values()
	for _, values := range [][]any{left, right} {
		for i, raw := range values {
			switch value := raw.(type) {
			case time.Time:
				values[i] = value.UTC().Truncate(time.Microsecond)
			case *time.Time:
				values[i] = canonicalBoardPageTime(value)
			case []time.Time:
				if value != nil {
					result := make([]time.Time, len(value))
					for j, at := range value {
						result[j] = at.UTC().Truncate(time.Microsecond)
					}
					values[i] = result
				}
			case []*time.Time:
				if value != nil {
					result := make([]*time.Time, len(value))
					for j, at := range value {
						result[j] = canonicalBoardPageTime(at)
					}
					values[i] = result
				}
			}
		}
	}
	return reflect.DeepEqual(left, right)
}

func canonicalBoardPageTime(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	value := at.UTC().Truncate(time.Microsecond)
	return &value
}
