package postgres_test

import (
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNeutralCoreCutoverRejectsUnimportedArtifactData(t *testing.T) {
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, populated := range []string{"", "artifact_services", "artifact_grants", "artifact_references", "missing_table"} {
			t.Run(string(org)+"_"+populated, func(t *testing.T) {
				var tables []postgres.NeutralImportTable
				records := make(map[string][]postgres.NeutralImportRow)
				for _, name := range []string{"artifact_services", "artifact_grants", "artifact_references"} {
					if populated == "missing_table" && name == "artifact_references" {
						continue
					}
					keys := []postgres.NeutralImportKey{{Column: "organization_id", Kind: "text"}, {Column: "project_id", Kind: "text"}}
					switch name {
					case "artifact_services":
						keys = append(keys, postgres.NeutralImportKey{Column: "id", Kind: "text"})
					case "artifact_grants":
						keys = []postgres.NeutralImportKey{{Column: "token_hash", Kind: "text"}}
					case "artifact_references":
						keys = append(keys, postgres.NeutralImportKey{Column: "artifact_id", Kind: "text"}, postgres.NeutralImportKey{Column: "revision", Kind: "integer"})
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
						records[name] = append(records[name], neutralFixtureRow(t, table, values))
					}
				}
				path, _ := writeNeutralFixture(t, org, tables, records)
				err := postgres.VerifyNeutralCoreCutover(t.Context(), path, org)
				if (err != nil) != (populated != "") {
					t.Fatalf("cutover source verification error=%v", err)
				}
			})
		}
	}
}
