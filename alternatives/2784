package postgres_test

import (
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNeutralCoreCutoverRejectsUnimportedArtifactData(t *testing.T) {
	t.Parallel()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, populated := range []string{"", "artifact_services", "artifact_grants", "artifact_references", "missing_table"} {
			t.Run(string(org)+"_"+populated, func(t *testing.T) {
				var tables []neutralimport.Table
				records := make(map[string][]neutralimport.Row)
				for _, name := range []string{"artifact_services", "artifact_grants", "artifact_references"} {
					if populated == "missing_table" && name == "artifact_references" {
						continue
					}
					keys := []neutralimport.Key{{Column: "organization_id", Kind: "text"}, {Column: "project_id", Kind: "text"}}
					switch name {
					case "artifact_services":
						keys = append(keys, neutralimport.Key{Column: "id", Kind: "text"})
					case "artifact_grants":
						keys = []neutralimport.Key{{Column: "token_hash", Kind: "text"}}
					case "artifact_references":
						keys = append(keys, neutralimport.Key{Column: "artifact_id", Kind: "text"}, neutralimport.Key{Column: "revision", Kind: "integer"})
					}
					table := neutralFixtureTable(t, name, keys)
					tables = append(tables, table)
					if name != populated {
						continue
					}
					for id := 1; id <= 2; id++ {
						values := make(map[string]any)
						if _, ok := table.Columns["organization_id"]; ok {
							values["organization_id"] = org
						}
						for _, key := range keys {
							if key.Column == "organization_id" {
								continue
							}
							values[key.Column] = fmt.Sprintf("example_%d", id)
							if key.Kind == "integer" {
								values[key.Column] = id
							}
						}
						records[name] = append(records[name], neutralimporttest.Row(t, table, values))
					}
				}
				path, _ := neutralimporttest.Write(t, []byte("historical source"), org, tables, records)
				err := postgres.VerifyNeutralCoreCutover(t.Context(), path, org)
				if (err != nil) != (populated != "") {
					t.Fatalf("cutover source verification error=%v", err)
				}
			})
		}
	}
}
