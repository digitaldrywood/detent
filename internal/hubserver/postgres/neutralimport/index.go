package neutralimport

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

type Index struct {
	bundle         *Bundle
	indexes        map[string]map[string][]indexEntry
	schema         storagecatalog.Schema
	versionChanges map[string]string
}

type indexEntry struct {
	key   []string
	order []string
}

func NewIndex(bundle *Bundle) *Index {
	return &Index{bundle: bundle, schema: bundle.Schema(), indexes: make(map[string]map[string][]indexEntry)}
}

func (s *Index) Bundle() *Bundle               { return s.bundle }
func (s *Index) Schema() storagecatalog.Schema { return s.schema }

func (s *Index) Rows(ctx context.Context, table string, filters map[string]string, order, after []string, limit int) ([]Row, error) {
	columns := make([]string, 0, len(filters))
	for column := range filters {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	descriptor, _ := json.Marshal([]any{table, columns, order})
	index, found := s.indexes[string(descriptor)]
	numeric := make([]bool, len(order))
	schema := s.schema[table]
	for i, column := range order {
		var signature []json.RawMessage
		if json.Unmarshal([]byte(schema[column]), &signature) != nil || len(signature) != 7 {
			return nil, ErrInvalid
		}
		var kind string
		if json.Unmarshal(signature[2], &kind) != nil {
			return nil, ErrInvalid
		}
		numeric[i] = strings.Contains(strings.ToUpper(kind), "INT")
	}
	compare := func(a, b []string) int {
		for i := range order {
			var result int
			if numeric[i] {
				left, _ := strconv.ParseInt(a[i], 10, 64)
				right, _ := strconv.ParseInt(b[i], 10, 64)
				result = cmp.Compare(left, right)
			} else {
				result = strings.Compare(a[i], b[i])
			}
			if result != 0 {
				return result
			}
		}
		return 0
	}
	if !found {
		index = make(map[string][]indexEntry)
		var cursor []string
		for {
			rows, err := s.bundle.Page(ctx, table, cursor, 100)
			if err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				values := make([]*string, len(columns))
				for i, column := range columns {
					cell, ok := row.Values[column]
					if !ok {
						derived, err := s.changeFilterValue(ctx, table, row, column)
						if err != nil {
							return nil, err
						}
						cell = derived
					}
					if !cell.Null && cell.Encoding != "utf8" {
						return nil, ErrInvalid
					}
					if !cell.Null {
						value := string(cell.Data)
						values[i] = &value
					}
				}
				group, _ := json.Marshal(values)
				entry := indexEntry{key: slices.Clone(row.Key), order: make([]string, len(order))}
				for i, column := range order {
					if err := row.Decode(map[string]any{column: &entry.order[i]}); err != nil {
						return nil, err
					}
					if numeric[i] {
						if _, err := strconv.ParseInt(entry.order[i], 10, 64); err != nil {
							return nil, ErrInvalid
						}
					}
				}
				index[string(group)] = append(index[string(group)], entry)
			}
			cursor = rows[len(rows)-1].Key
		}
		for group, entries := range index {
			slices.SortFunc(entries, func(a, b indexEntry) int { return compare(a.order, b.order) })
			for i := 1; i < len(entries); i++ {
				if compare(entries[i-1].order, entries[i].order) == 0 {
					return nil, fmt.Errorf("ambiguous neutral %s order: %w", table, ErrInvalid)
				}
			}
			index[group] = entries
		}
		s.indexes[string(descriptor)] = index
	}
	values := make([]string, len(columns))
	for i, column := range columns {
		values[i] = filters[column]
	}
	group, _ := json.Marshal(values)
	entries := index[string(group)]
	start := 0
	if len(after) != 0 {
		if len(after) != len(order) {
			return nil, ErrInvalid
		}
		start = sort.Search(len(entries), func(i int) bool { return compare(entries[i].order, after) > 0 })
	}
	if limit < 1 {
		return nil, ErrInvalid
	}
	end := min(start+limit, len(entries))
	result := make([]Row, 0, end-start)
	for _, entry := range entries[start:end] {
		row, err := s.bundle.Lookup(ctx, table, entry.key)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func (s *Index) One(ctx context.Context, table string, filters map[string]string, order []string) (Row, error) {
	rows, err := s.Rows(ctx, table, filters, order, nil, 2)
	if err != nil {
		return Row{}, err
	}
	if len(rows) == 0 {
		return Row{}, sql.ErrNoRows
	}
	if len(rows) != 1 {
		return Row{}, errors.Join(ErrInvalid, errors.New("ambiguous neutral record"))
	}
	return rows[0], nil
}

func (s *Index) Each(ctx context.Context, table string, filters map[string]string, order []string, read func(Row) error) error {
	var after []string
	for {
		rows, err := s.Rows(ctx, table, filters, order, after, 100)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if err := read(row); err != nil {
				return err
			}
		}
		last := rows[len(rows)-1]
		after = make([]string, len(order))
		for index, column := range order {
			if err := last.Decode(map[string]any{column: &after[index]}); err != nil {
				return err
			}
		}
	}
}

func (s *Index) changeFilterValue(ctx context.Context, table string, row Row, column string) (storagecatalog.Cell, error) {
	if column != "change_id" || table != "change_landing_receipts" && table != "change_viewed_files" && table != "quality_landings" {
		return storagecatalog.Cell{}, ErrInvalid
	}
	if s.versionChanges == nil {
		owners := make(map[string]string)
		var after []string
		for {
			rows, err := s.bundle.Page(ctx, "change_versions", after, 100)
			if err != nil {
				return storagecatalog.Cell{}, err
			}
			if len(rows) == 0 {
				break
			}
			for _, version := range rows {
				var id, change string
				if err := version.Decode(map[string]any{"id": &id, "change_id": &change}); err != nil {
					return storagecatalog.Cell{}, err
				}
				owners[id] = change
			}
			after = rows[len(rows)-1].Key
		}
		s.versionChanges = owners
	}
	var version string
	if err := row.Decode(map[string]any{"version_id": &version}); err != nil {
		return storagecatalog.Cell{}, err
	}
	owner, ok := s.versionChanges[version]
	if !ok || owner == "" {
		return storagecatalog.Cell{}, eventstore.ErrInvalidRecord
	}
	return storagecatalog.Cell{Encoding: "utf8", Data: []byte(owner)}, nil
}
