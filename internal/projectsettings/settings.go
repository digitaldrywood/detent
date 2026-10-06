package projectsettings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrConflict = errors.New("settings revision changed")
var ErrInvalid = errors.New("invalid settings")

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type Rank struct {
	Revision   int64               `json:"revision"`
	ProjectIDs []tracker.ProjectID `json:"project_ids"`
}

type RankChange struct {
	ExpectedRevision int64               `json:"expected_revision"`
	ProjectIDs       []tracker.ProjectID `json:"project_ids"`
}

func ReadRank(ctx context.Context, db Queryer, organization tracker.OrganizationID) (Rank, error) {
	value := Rank{ProjectIDs: []tracker.ProjectID{}}
	if err := db.QueryRowContext(ctx, "SELECT scheduling_revision FROM organizations WHERE id = ?", organization).Scan(&value.Revision); err != nil {
		return value, err
	}
	rows, err := db.QueryContext(ctx, "SELECT id FROM projects WHERE organization_id = ? ORDER BY scheduling_rank, created_at, id", organization)
	if err != nil {
		return value, err
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.ProjectID
		if err := rows.Scan(&id); err != nil {
			return value, err
		}
		value.ProjectIDs = append(value.ProjectIDs, id)
	}
	return value, rows.Err()
}

func UpdateRank(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, change RankChange) (Rank, error) {
	current, err := ReadRank(ctx, tx, organization)
	if err != nil {
		return current, err
	}
	if current.Revision != change.ExpectedRevision {
		return current, ErrConflict
	}
	if len(change.ProjectIDs) != len(current.ProjectIDs) {
		return current, ErrInvalid
	}
	allowed := map[tracker.ProjectID]bool{}
	for _, id := range current.ProjectIDs {
		allowed[id] = true
	}
	for _, id := range change.ProjectIDs {
		if !allowed[id] {
			return current, ErrInvalid
		}
		delete(allowed, id)
	}
	for rank, id := range change.ProjectIDs {
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET scheduling_rank = ? WHERE organization_id = ? AND id = ?", rank, organization, id); err != nil {
			return current, err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE organizations SET scheduling_revision = scheduling_revision + 1 WHERE id = ?", organization); err != nil {
		return current, err
	}
	return ReadRank(ctx, tx, organization)
}

type ModelSelection struct {
	Revision  tracker.Revision       `json:"revision,string"`
	Selection *config.ModelSelection `json:"selection"`
	Effective config.ModelSelection  `json:"effective"`
}

type ModelSelectionChange struct {
	ExpectedRevision tracker.Revision       `json:"expected_revision,string"`
	Selection        *config.ModelSelection `json:"selection"`
}

func ReadModelSelection(ctx context.Context, db Queryer, organization tracker.OrganizationID, project tracker.ProjectID) (ModelSelection, error) {
	var result ModelSelection
	var raw string
	var org config.ModelSelection
	if err := db.QueryRowContext(ctx, "SELECT model_selection_json, model_selection_revision FROM organizations WHERE id=?", organization).Scan(&raw, &result.Revision); err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(raw), &org); err != nil {
		return result, err
	}
	result.Selection = &org
	var override config.ModelSelection
	if project != "" {
		var raw sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT model_selection_json, model_selection_revision FROM projects WHERE organization_id=? AND id=?", organization, project).Scan(&raw, &result.Revision); err != nil {
			return result, err
		}
		result.Selection = nil
		if raw.Valid {
			if err := json.Unmarshal([]byte(raw.String), &override); err != nil {
				return result, err
			}
			result.Selection = &override
		}
	}
	result.Effective = config.ResolveCloudModelSelection(org, override)
	return result, nil
}

func UpdateModelSelection(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, project tracker.ProjectID, change ModelSelectionChange) (ModelSelection, error) {
	current, err := ReadModelSelection(ctx, tx, organization, project)
	if err != nil {
		return current, err
	}
	if current.Revision != change.ExpectedRevision {
		return current, ErrConflict
	}
	if project == "" && change.Selection == nil {
		return current, ErrInvalid
	}
	var raw any
	if change.Selection != nil {
		encoded, err := json.Marshal(change.Selection)
		if err != nil {
			return current, err
		}
		raw = string(encoded)
	}
	if project == "" {
		_, err = tx.ExecContext(ctx, "UPDATE organizations SET model_selection_json=?, model_selection_revision=model_selection_revision+1 WHERE id=?", raw, organization)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE projects SET model_selection_json=?, model_selection_revision=model_selection_revision+1 WHERE organization_id=? AND id=?", raw, organization, project)
	}
	if err != nil {
		return current, err
	}
	saved, err := ReadModelSelection(ctx, tx, organization, project)
	if err != nil {
		return current, err
	}
	if problems := saved.Effective.Validate(); len(problems) > 0 {
		return current, errors.Join(ErrInvalid, errors.New(strings.Join(problems, "; ")))
	}
	return saved, nil
}
