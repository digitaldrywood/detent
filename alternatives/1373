package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const importedDependencyID = `COALESCE(r.dependency_id,(SELECT d.dependency_id FROM legacy_import_dependencies d WHERE d.sequence=r.sequence AND d.record_payload=r.record_json))`

func legacyImportDependencies(ctx context.Context, query nativeQueryer, project tracker.ProjectID) (string, []any, error) {
	rows, err := query.QueryContext(ctx, `SELECT r.sequence,r.record_json FROM github_import_records r JOIN github_imports g ON g.id=r.import_id WHERE g.project_id=? AND r.kind='dependency' AND r.current_dependency=1 AND r.dependency_id IS NULL`, project)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var placeholders []string
	var args []any
	for rows.Next() {
		var sequence int64
		var raw string
		if err := rows.Scan(&sequence, &raw); err != nil {
			return "", nil, err
		}
		fields, err := evaluatedHostedImportFields(ctx, query, "github_import_records", raw)
		if err != nil {
			return "", nil, err
		}
		placeholders = append(placeholders, "(?,?,?)")
		args = append(args, sequence, raw, fields.dependencyID)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	body := `SELECT NULL,NULL,NULL WHERE 0`
	if len(placeholders) > 0 {
		body = "VALUES " + strings.Join(placeholders, ",")
	}
	return `WITH legacy_import_dependencies(sequence,record_payload,dependency_id) AS (` + body + `) `, args, nil
}
