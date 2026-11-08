package postgres_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func accessNeutralBundle(t *testing.T, source postgres.AccessImportSource, includeSecond bool) string {
	t.Helper()
	var tables []postgres.NeutralImportTable
	records := make(map[string][]postgres.NeutralImportRow)
	for _, name := range accessFixtureTables {
		table := neutralFixtureTable(t, name, nil)
		var columns []string
		defaults := make(map[string]bool)
		for name, signature := range table.Columns {
			var fields []json.RawMessage
			if err := json.Unmarshal([]byte(signature), &fields); err != nil || len(fields) != 7 {
				t.Fatalf("source column signature: %v", err)
			}
			var primary, required int
			var kind string
			var fallback *string
			if err := json.Unmarshal(fields[5], &primary); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(fields[2], &kind); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(fields[3], &required); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(fields[4], &fallback); err != nil {
				t.Fatal(err)
			}
			defaults[name] = required != 0 && fallback != nil
			if primary > 0 {
				for len(table.Key) < primary {
					table.Key = append(table.Key, postgres.NeutralImportKey{})
				}
				keyKind := "text"
				if strings.Contains(kind, "INT") {
					keyKind = "integer"
				}
				table.Key[primary-1] = postgres.NeutralImportKey{Column: name, Kind: keyKind}
			}
			columns = append(columns, name)
		}
		slices.Sort(columns)
		quoted := make([]string, len(columns))
		for index, column := range columns {
			quoted[index] = pgx.Identifier{column}.Sanitize()
		}
		rows, err := source.DB.QueryContext(t.Context(), "SELECT "+strings.Join(quoted, ",")+" FROM "+pgx.Identifier{source.Source.Schema, name}.Sanitize())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			named := make(map[string]any, len(columns))
			for index, value := range values {
				if value == nil && defaults[columns[index]] {
					continue
				}
				if value != nil {
					switch value.(type) {
					case string, []byte:
					default:
						value = fmt.Sprint(value)
					}
				}
				named[columns[index]] = value
			}
			records[name] = append(records[name], neutralFixtureRow(t, table, named))
			if includeSecond && name != "organizations" && name != "hosted_tenant" {
				second := maps.Clone(named)
				for column, value := range second {
					text, ok := value.(string)
					if !ok || column == "organization_id" || text == source.Source.OrganizationID || text == "provider_first" {
						continue
					}
					text = strings.ReplaceAll(text, "_first", "_second")
					text = strings.ReplaceAll(text, "provider_second", "provider_first")
					if column == "email" {
						text = strings.Replace(text, "@", "_second@", 1)
					}
					if column == "token" {
						text = fmt.Sprint(value) + "_second"
					}
					if column == "name" {
						text += " second"
					}
					second[column] = text
				}
				for _, key := range table.Key {
					if key.Kind == "integer" {
						value, err := strconv.ParseInt(fmt.Sprint(named[key.Column]), 10, 64)
						if err != nil {
							t.Fatal(err)
						}
						second[key.Column] = value + 1
					}
				}
				if name == "organization_secrets" {
					second["kind"] = "slack"
				}
				records[name] = append(records[name], neutralFixtureRow(t, table, second))
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	pageSize := 100
	if includeSecond {
		pageSize = 1
	}
	path, _ := writeNeutralFixture(t, tracker.OrganizationID(source.Source.OrganizationID), tables, records, pageSize)
	return path
}
