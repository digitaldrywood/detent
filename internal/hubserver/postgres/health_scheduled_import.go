package postgres

import (
	"slices"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func ValidateNeutralHealthScheduledSource(source *VerifiedNeutralImport) error {
	if source == nil || source.Identity().SchemaVersion != 1 {
		return storagecatalog.ErrUnmapped
	}
	columns := map[string]string{
		"organization_id": `[0,"organization_id","TEXT",1,null,1,0]`,
		"project_id":      `[1,"project_id","TEXT",1,null,2,0]`,
		"repository":      `[2,"repository","TEXT",1,null,3,0]`,
		"run_id":          `[3,"run_id","TEXT",1,null,4,0]`,
		"run_attempt":     `[4,"run_attempt","TEXT",1,null,5,0]`,
		"record_json":     `[5,"record_json","TEXT",1,null,0,0]`,
		"recorded_at":     `[6,"recorded_at","TEXT",1,null,0,0]`,
		"conclusion":      `[7,"conclusion","TEXT",0,null,0,0]`,
	}
	table, err := source.table("scheduled_validation_reports")
	if err != nil || len(table.Columns) != len(columns) || !slices.Equal(table.Key, []NeutralImportKey{{Column: "organization_id", Kind: "text"}, {Column: "project_id", Kind: "text"}, {Column: "repository", Kind: "text"}, {Column: "run_id", Kind: "text"}, {Column: "run_attempt", Kind: "text"}}) {
		return storagecatalog.ErrUnmapped
	}
	for column, signature := range columns {
		if !sameSourceSignature([]byte(table.Columns[column]), signature) {
			return storagecatalog.ErrUnmapped
		}
	}
	return ValidateNeutralCoreImportTables(source, "projects", "repositories", "issues", "native_comments")
}
