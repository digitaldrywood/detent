package hubserver

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

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coreNeutralImport struct {
	bundle  *postgres.VerifiedNeutralImport
	indexes map[string]map[string][]coreNeutralIndexEntry
}

type coreNeutralIndexEntry struct {
	key   []string
	order []string
}

func newCoreNeutralImport(bundle *postgres.VerifiedNeutralImport) *coreNeutralImport {
	return &coreNeutralImport{bundle: bundle, indexes: make(map[string]map[string][]coreNeutralIndexEntry)}
}

func (s *coreNeutralImport) rows(ctx context.Context, table string, filters map[string]string, order, after []string, limit int) ([]postgres.NeutralImportRow, error) {
	columns := make([]string, 0, len(filters))
	for column := range filters {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	descriptor, _ := json.Marshal([]any{table, columns, order})
	index, found := s.indexes[string(descriptor)]
	numeric := make([]bool, len(order))
	schema := s.bundle.Schema()[table]
	for i, column := range order {
		var signature []json.RawMessage
		if json.Unmarshal([]byte(schema[column]), &signature) != nil || len(signature) != 7 {
			return nil, postgres.ErrNeutralImport
		}
		var kind string
		if json.Unmarshal(signature[2], &kind) != nil {
			return nil, postgres.ErrNeutralImport
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
		index = make(map[string][]coreNeutralIndexEntry)
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
					if !ok || !cell.Null && cell.Encoding != "utf8" {
						return nil, postgres.ErrNeutralImport
					}
					if !cell.Null {
						value := string(cell.Data)
						values[i] = &value
					}
				}
				group, _ := json.Marshal(values)
				entry := coreNeutralIndexEntry{key: slices.Clone(row.Key), order: make([]string, len(order))}
				for i, column := range order {
					if err := row.Decode(map[string]any{column: &entry.order[i]}); err != nil {
						return nil, err
					}
					if numeric[i] {
						if _, err := strconv.ParseInt(entry.order[i], 10, 64); err != nil {
							return nil, postgres.ErrNeutralImport
						}
					}
				}
				index[string(group)] = append(index[string(group)], entry)
			}
			cursor = rows[len(rows)-1].Key
		}
		for group, entries := range index {
			slices.SortFunc(entries, func(a, b coreNeutralIndexEntry) int { return compare(a.order, b.order) })
			for i := 1; i < len(entries); i++ {
				if compare(entries[i-1].order, entries[i].order) == 0 {
					return nil, fmt.Errorf("ambiguous neutral %s order: %w", table, postgres.ErrNeutralImport)
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
			return nil, postgres.ErrNeutralImport
		}
		start = sort.Search(len(entries), func(i int) bool { return compare(entries[i].order, after) > 0 })
	}
	if limit < 1 {
		return nil, postgres.ErrNeutralImport
	}
	end := min(start+limit, len(entries))
	result := make([]postgres.NeutralImportRow, 0, end-start)
	for _, entry := range entries[start:end] {
		row, err := s.bundle.Lookup(ctx, table, entry.key)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func (s *coreNeutralImport) one(ctx context.Context, table string, filters map[string]string, order []string) (postgres.NeutralImportRow, error) {
	rows, err := s.rows(ctx, table, filters, order, nil, 2)
	if err != nil {
		return postgres.NeutralImportRow{}, err
	}
	if len(rows) == 0 {
		return postgres.NeutralImportRow{}, sql.ErrNoRows
	}
	if len(rows) != 1 {
		return postgres.NeutralImportRow{}, errors.Join(postgres.ErrNeutralImport, errors.New("ambiguous neutral record"))
	}
	return rows[0], nil
}

func readCoreNeutralObject(ctx context.Context, source *coreNeutralImport, organization tracker.OrganizationID, objects attachment.DetailStore, key string) ([]byte, error) {
	row, err := source.one(ctx, "change_content_objects", map[string]string{"organization_id": string(organization), "object_key": key}, []string{"object_key"})
	ref := contentReference()
	if errors.Is(err, sql.ErrNoRows) {
		row, err = source.one(ctx, "attachments", map[string]string{"organization_id": string(organization), "object_key": key, "kind": "archive", "customer_owned": "0", "storage_binding_id": ""}, []string{"object_key"})
		if err != nil {
			return nil, err
		}
		err = row.Decode(map[string]any{"object_key": &ref.Key, "sha256": &ref.SHA256, "size": &ref.CompressedSize, "uncompressed_size": &ref.UncompressedSize})
	} else if err == nil {
		err = row.Decode(map[string]any{"object_key": &ref.Key, "object_sha256": &ref.SHA256, "object_bytes": &ref.CompressedSize, "content_bytes": &ref.UncompressedSize})
	}
	if err != nil {
		return nil, err
	}
	if err := ref.Validate(string(organization)); err != nil {
		return nil, err
	}
	raw, err := objects.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return raw, nil
}
