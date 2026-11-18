package boardviewsql

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
)

type arrayRow struct {
	text  [8][]*string
	ints  [2][]*int64
	bools [3][]*bool
	times [2][]*time.Time
}

func (a *arrayRow) targets() []any {
	targets := make([]any, 0, len(boardViewArrayColumns))
	for i := range a.text {
		targets = append(targets, &a.text[i])
	}
	for i := range a.ints {
		targets = append(targets, &a.ints[i])
	}
	for i := range a.bools {
		targets = append(targets, &a.bools[i])
	}
	for i := range a.times {
		targets = append(targets, &a.times[i])
	}
	return targets
}

type expansion struct {
	kind        string
	project, id int
	text        []int
	ints        []int
	bools       []int
	times       []int
	readable    bool
}

var expansions = map[string]expansion{
	"links":        {kind: "link", project: 1, id: 2, text: []int{0, 3, 4, 5, 6, 7}, ints: []int{0, 1}, bools: []int{0, 1}, times: []int{0, 1}, readable: true},
	"dependencies": {kind: "dependency", project: 0, id: 1, text: []int{-1, 2}, ints: []int{-1, 0}, readable: true},
	"reasons":      {kind: "reason", project: 0, id: 1, text: []int{2, 3, 4, 5, 6, 7}, ints: []int{0, 1}, bools: []int{0}},
	"states":       {kind: "state", project: 0, id: 1, text: []int{2}, ints: []int{0, 1}, bools: []int{0, 1, 2}},
}

func (e expansion) rows(a *arrayRow) ([]viewstore.BoardViewRow, error) {
	n := len(a.text[e.project])
	lengths := []int{len(a.text[e.id])}
	for _, i := range e.text {
		if i >= 0 {
			lengths = append(lengths, len(a.text[i]))
		}
	}
	for _, i := range e.ints {
		if i >= 0 {
			lengths = append(lengths, len(a.ints[i]))
		}
	}
	for _, i := range e.bools {
		lengths = append(lengths, len(a.bools[i]))
	}
	for _, i := range e.times {
		lengths = append(lengths, len(a.times[i]))
	}
	for _, length := range lengths {
		if length != n {
			return nil, fmt.Errorf("board %s arrays differ in length: %w", e.kind, viewstore.ErrInvalidView)
		}
	}
	readable := true
	rows := make([]viewstore.BoardViewRow, n)
	for element := range rows {
		row := &rows[element]
		row.Kind, row.Project, row.ID = e.kind, a.text[e.project][element], a.text[e.id][element]
		for slot, i := range e.text {
			if i >= 0 {
				row.Text[slot] = a.text[i][element]
			}
		}
		for slot, i := range e.ints {
			if i >= 0 {
				row.Int[slot] = a.ints[i][element]
			}
		}
		for slot, i := range e.bools {
			row.Bool[slot] = a.bools[i][element]
		}
		for slot, i := range e.times {
			row.Time[slot] = a.times[i][element]
		}
		if e.readable {
			row.Bool[2] = &readable
		}
	}
	return rows, nil
}

func Scan(rows pgx.Rows) (viewstore.BoardViewRows, error) {
	var result viewstore.BoardViewRows
	defer rows.Close()
	for rows.Next() {
		var row viewstore.BoardViewRow
		var arrays arrayRow
		if err := rows.Scan(append(row.Targets(), arrays.targets()...)...); err != nil {
			return viewstore.BoardViewRows{}, err
		}
		expand, aggregated := expansions[row.Kind]
		if !aggregated {
			result.Rows = append(result.Rows, row)
			continue
		}
		expanded, err := expand.rows(&arrays)
		if err != nil {
			return viewstore.BoardViewRows{}, err
		}
		result.Rows = append(result.Rows, expanded...)
	}
	if err := rows.Err(); err != nil {
		return viewstore.BoardViewRows{}, err
	}
	return result, nil
}
