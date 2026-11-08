package postgres

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func (s *VerifiedNeutralImport) ValidateCatalogTable(catalog storagecatalog.Catalog, schema storagecatalog.Schema, name string, keys []string) error {
	table, err := s.table(name)
	if err != nil {
		return err
	}
	definition, ok := catalog.Tables["hub/"+name]
	if !ok || len(table.Columns) != len(definition.Columns) || len(schema[name]) != len(definition.Columns) || len(table.Key) != len(keys) {
		return storagecatalog.ErrUnmapped
	}
	for column, mapping := range definition.Columns {
		if schema[name][column] != mapping.Source || !sameSourceSignature([]byte(table.Columns[column]), mapping.Source) {
			return storagecatalog.ErrUnmapped
		}
	}
	for index, key := range keys {
		if table.Key[index].Column != key {
			return storagecatalog.ErrUnmapped
		}
		var fields []json.RawMessage
		if json.Unmarshal([]byte(table.Columns[key]), &fields) != nil || len(fields) != 7 {
			return storagecatalog.ErrUnmapped
		}
		var kind string
		if json.Unmarshal(fields[2], &kind) != nil {
			return storagecatalog.ErrUnmapped
		}
		expected := "text"
		if strings.Contains(strings.ToUpper(kind), "INT") {
			expected = "integer"
		}
		if table.Key[index].Kind != expected {
			return storagecatalog.ErrUnmapped
		}
	}
	for name := range table.Columns {
		if _, ok := definition.Columns[name]; !ok {
			return storagecatalog.ErrUnmapped
		}
	}
	var primary []struct {
		position int
		name     string
	}
	for name, signature := range table.Columns {
		var fields []json.RawMessage
		if json.Unmarshal([]byte(signature), &fields) != nil || len(fields) != 7 {
			return storagecatalog.ErrUnmapped
		}
		var position int
		if json.Unmarshal(fields[5], &position) != nil {
			return storagecatalog.ErrUnmapped
		}
		if position > 0 {
			primary = append(primary, struct {
				position int
				name     string
			}{position, name})
		}
	}
	slices.SortFunc(primary, func(a, b struct {
		position int
		name     string
	}) int {
		return a.position - b.position
	})
	if len(primary) != len(keys) {
		return storagecatalog.ErrUnmapped
	}
	for index, column := range primary {
		if column.position != index+1 || column.name != keys[index] {
			return storagecatalog.ErrUnmapped
		}
	}
	return nil
}
