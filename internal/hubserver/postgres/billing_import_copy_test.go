package postgres_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type neutralBillingFixture struct {
	organization tracker.OrganizationID
	tables       []postgres.NeutralImportTable
	records      map[string][]postgres.NeutralImportRow
}

func newNeutralBillingFixture(t *testing.T, organization tracker.OrganizationID, names ...string) *neutralBillingFixture {
	t.Helper()
	fixture := &neutralBillingFixture{organization: organization, records: map[string][]postgres.NeutralImportRow{}}
	for _, name := range names {
		table := neutralFixtureTable(t, name, nil)
		positions := map[int]postgres.NeutralImportKey{}
		for column, signature := range table.Columns {
			var fields []json.RawMessage
			if err := json.Unmarshal([]byte(signature), &fields); err != nil {
				t.Fatal(err)
			}
			var position int
			var kind string
			if err := json.Unmarshal(fields[5], &position); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(fields[2], &kind); err != nil {
				t.Fatal(err)
			}
			if position == 0 {
				continue
			}
			keyKind := "text"
			if strings.Contains(strings.ToUpper(kind), "INT") {
				keyKind = "integer"
			}
			positions[position] = postgres.NeutralImportKey{Column: column, Kind: keyKind}
		}
		for position := 1; position <= len(positions); position++ {
			key, ok := positions[position]
			if !ok {
				t.Fatal("non-contiguous primary key")
			}
			table.Key = append(table.Key, key)
		}
		if len(table.Key) == 0 {
			t.Fatal("missing primary key")
		}
		fixture.tables = append(fixture.tables, table)
	}
	return fixture
}
func (f *neutralBillingFixture) add(t *testing.T, name string, values map[string]any) {
	t.Helper()
	for _, table := range f.tables {
		if table.Name == name {
			row := neutralFixtureRow(t, table, values)
			if row.SourceRowID == 0 {
				row.SourceRowID = int64(len(f.records[name])) + 1
			}
			f.records[name] = append(f.records[name], row)
			return
		}
	}
	t.Fatal("missing fixture table")
}
func (f *neutralBillingFixture) seal(t *testing.T) string {
	t.Helper()
	directory, _ := writeNeutralFixture(t, f.organization, f.tables, f.records)
	return directory
}
