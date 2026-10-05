package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func readNativeSourceReferences(ctx context.Context, query nativeQueryer, scope nativeScope, id string) ([]tracker.ExternalReference, error) {
	var nodeID, owner, repository, sourceURL string
	var number int
	var provenance sql.NullString
	err := query.QueryRowContext(ctx, `SELECT COALESCE(i.github_node_id, ''), COALESCE(r.github_owner, ''), COALESCE(r.github_name, ''),
COALESCE(i.github_number, 0), COALESCE(l.source_url, ''), i.provenance_json
FROM issues i LEFT JOIN repositories r ON r.id = i.repository_id
LEFT JOIN linked_issue_sources l ON l.work_item_id = i.native_id
WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, scope.organization, scope.project, id).Scan(&nodeID, &owner, &repository, &number, &sourceURL, &provenance)
	if err != nil {
		return nil, err
	}
	references := []tracker.ExternalReference{}
	if sourceURL != "" {
		references = append(references, tracker.GitHubIssueSourceReference(sourceURL, sourceURL))
	}
	if number > 0 && owner != "" && repository != "" {
		sourceURL = "https://github.com/" + owner + "/" + repository + "/issues/" + strconv.Itoa(number)
	}
	var source *tracker.Provenance
	if provenance.Valid {
		if err := json.Unmarshal([]byte(provenance.String), &source); err != nil {
			return nil, err
		}
	}
	if source != nil {
		if source.Provider == "github" {
			references = append(references, tracker.GitHubIssueSourceReference(source.ExternalID, sourceURL))
		} else {
			references = append(references, tracker.ExternalReference{Provider: source.Provider, Kind: "issue", ID: source.ExternalID})
		}
	} else if nodeID != "" || number > 0 {
		if nodeID == "" {
			nodeID = sourceURL
		}
		references = append(references, tracker.GitHubIssueSourceReference(nodeID, sourceURL))
	}
	return references, nil
}

func readChangeSourceIssues(ctx context.Context, query nativeQueryer, scope nativeScope, changeID string) ([]tracker.ExternalReference, error) {
	rows, err := query.QueryContext(ctx, `SELECT work_item_id FROM change_issue_links
WHERE organization_id = ? AND project_id = ? AND change_id = ? ORDER BY work_item_id`, scope.organization, scope.project, changeID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var sources []tracker.ExternalReference
	seen := make(map[string]bool)
	for _, id := range ids {
		references, err := readNativeSourceReferences(ctx, query, scope, id)
		if err != nil {
			return nil, err
		}
		for _, reference := range references {
			if reference.Provider == "github" && reference.URL != "" && !seen[reference.URL] {
				sources = append(sources, reference)
				seen[reference.URL] = true
			}
		}
	}
	return sources, nil
}
