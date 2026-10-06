package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var githubMigrationURL = regexp.MustCompile(`https://github\.com/[^\s<>()\[\]]+`)

func githubMigrationSourceURL(body string) string {
	var candidate string
	section := false
	for line := range strings.SplitSeq(body, "\n") {
		label := strings.TrimLeft(strings.TrimSpace(line), "#*- ")
		if strings.HasPrefix(strings.ToLower(label), "migration context") {
			rest := strings.TrimSpace(label[len("migration context"):])
			if rest == "" || strings.HasPrefix(rest, ":") || rest == "**" {
				section = strings.HasPrefix(strings.TrimSpace(line), "#")
			} else {
				continue
			}
		} else if strings.HasPrefix(strings.TrimSpace(line), "#") {
			section = false
			continue
		} else if !section {
			continue
		}
		for _, raw := range githubMigrationURL.FindAllString(line, -1) {
			canonical, _, _, err := tracker.ParseGitHubIssueURL(strings.TrimRight(raw, "`*.,;"))
			if err != nil {
				continue
			}
			if candidate != "" && candidate != canonical {
				return ""
			}
			candidate = canonical
		}
	}
	return candidate
}

func migrateGitHubIssueReferences(ctx context.Context, tx *sql.Tx, logger *slog.Logger) error {
	rows, err := tx.QueryContext(ctx, `SELECT i.native_id, i.project_id, i.body, i.url,
COALESCE(i.github_number, 0), COALESCE(r.github_owner, ''), COALESCE(r.github_name, ''),
COALESCE(json_extract(i.provenance_json, '$.external_id'), i.github_node_id, ''), COALESCE(l.source_url, '')
FROM issues i LEFT JOIN repositories r ON r.id = i.repository_id
LEFT JOIN linked_issue_sources l ON l.work_item_id = i.native_id
WHERE json_extract(i.provenance_json, '$.provider') = 'github' OR i.github_node_id IS NOT NULL
ORDER BY i.project_id, i.native_id`)
	if err != nil {
		return err
	}
	type item struct {
		id, project, body, url, owner, repository, nodeID, linkedURL string
		number                                                       int
	}
	var items []item
	for rows.Next() {
		var value item
		if err := rows.Scan(&value.id, &value.project, &value.body, &value.url, &value.number, &value.owner, &value.repository, &value.nodeID, &value.linkedURL); err != nil {
			return errors.Join(err, rows.Close())
		}
		refs := nativeSourceReferences(value.nodeID, value.owner, value.repository, value.number, value.linkedURL, value.url, &tracker.Provenance{Provider: "github", ExternalID: value.nodeID})
		if refs[len(refs)-1].Number == 0 {
			items = append(items, value)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	resolved := 0
	for _, value := range items {
		urls, err := importedGitHubIssueURLs(ctx, tx, value.project, value.id, value.nodeID)
		if err != nil {
			return fmt.Errorf("read GitHub source for %s: %w", value.id, err)
		}
		urls = append(urls, value.url, value.linkedURL, githubMigrationSourceURL(value.body))
		var reference tracker.ExternalReference
		conflict := false
		for _, raw := range urls {
			next := tracker.GitHubIssueSourceReference(value.nodeID, raw)
			if next.URL == "" {
				continue
			}
			if reference.URL != "" && reference.URL != next.URL {
				conflict = true
			}
			reference = next
		}
		if value.number > 0 && reference.Number != value.number {
			conflict = true
		}
		if value.owner != "" && value.repository != "" && !strings.EqualFold(value.owner+"/"+value.repository, reference.Repository) {
			conflict = true
		}
		if reference.URL == "" || conflict {
			logger.WarnContext(ctx, "GitHub issue reference remains unresolved", "project_id", value.project, "work_item_id", value.id, "node_id", value.nodeID, "conflicting_sources", conflict)
			continue
		}
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET url = ?, github_number = ? WHERE native_id = ?", reference.URL, reference.Number, value.id); err != nil {
			return fmt.Errorf("backfill GitHub source for %s: %w", value.id, err)
		}
		resolved++
	}
	logger.InfoContext(ctx, "Backfilled imported GitHub issue references", "resolved", resolved, "unresolved", len(items)-resolved)
	return nil
}

func importedGitHubIssueURLs(ctx context.Context, tx *sql.Tx, project, item, nodeID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.record_json FROM github_import_records r
JOIN github_imports g ON g.id = r.import_id
WHERE g.project_id = ? AND g.work_item_id = ? AND r.kind = 'issue'
UNION ALL SELECT record_json FROM collaboration_versions
WHERE project_id = ? AND work_item_id = ? AND record_id = ?`, project, item, project, item, item)
	if err != nil {
		return nil, err
	}
	var urls []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var record struct {
			Body       string             `json:"body"`
			Provenance tracker.Provenance `json:"provenance"`
			Data       struct {
				NodeID  string `json:"node_id"`
				URL     string `json:"url"`
				HTMLURL string `json:"html_url"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if record.Provenance.ExternalID != "" && record.Provenance.ExternalID != nodeID || record.Data.NodeID != "" && record.Data.NodeID != nodeID {
			continue
		}
		urls = append(urls, record.Data.HTMLURL, record.Data.URL, githubMigrationSourceURL(record.Body))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return urls, nil
}
