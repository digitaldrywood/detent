package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const LocalOrganization tracker.OrganizationID = "local"
const LocalRunner = "local"

type LocalConfigurationStore interface {
	InitializeLocalConfiguration(context.Context, globalconfig.Config, *config.ModelSelection) (bool, error)
	LocalConfiguration(context.Context, globalconfig.Config) (globalconfig.Config, error)
	MutateLocalConfiguration(context.Context, globalconfig.Config, func(*globalconfig.Config, string) bool) error
	LocalProjectRank(context.Context, *projectsettings.RankChange) (projectsettings.Rank, error)
	LocalModelSelection(context.Context, tracker.ProjectID, *projectsettings.ModelSelectionChange) (projectsettings.ModelSelection, error)
	LocalAllowedProjects(context.Context, *AllowedProjectsChange) (AllowedProjects, error)
}

type AllowedProjects struct {
	Revision   int64               `json:"revision"`
	ProjectIDs []tracker.ProjectID `json:"project_ids"`
}

type AllowedProjectsChange struct {
	ExpectedRevision int64               `json:"expected_revision"`
	ProjectIDs       []tracker.ProjectID `json:"project_ids"`
}

func (s *sqliteStore) InitializeLocalConfiguration(ctx context.Context, cfg globalconfig.Config, selection *config.ModelSelection) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "INSERT INTO organizations(id) VALUES (?) ON CONFLICT(id) DO NOTHING", LocalOrganization)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	projects := slices.Clone(cfg.Projects)
	slices.SortStableFunc(projects, func(a, b globalconfig.Project) int {
		if a.Priority > b.Priority {
			return -1
		}
		if a.Priority < b.Priority {
			return 1
		}
		return 0
	})
	for rank, project := range projects {
		raw, err := json.Marshal(project)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO projects(id,organization_id,configuration_json,scheduling_rank,created_at) VALUES (?,?,?,?,?)", project.ID, LocalOrganization, string(raw), rank, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,?,?)", LocalRunner, LocalOrganization, project.ID); err != nil {
			return false, err
		}
	}
	if selection != nil {
		if _, err := projectsettings.UpdateModelSelection(ctx, tx, LocalOrganization, "", projectsettings.ModelSelectionChange{ExpectedRevision: 1, Selection: selection}); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func localConfiguration(ctx context.Context, db projectsettings.Queryer, cfg globalconfig.Config) (globalconfig.Config, int64, error) {
	var revision int64
	if err := db.QueryRowContext(ctx, "SELECT configuration_revision FROM organizations WHERE id=?", LocalOrganization).Scan(&revision); err != nil {
		return cfg, revision, err
	}
	selection, err := projectsettings.ReadModelSelection(ctx, db, LocalOrganization, "")
	if err != nil {
		return cfg, revision, err
	}
	cfg.Global.Agents.ModelSelection = selection.Effective
	rows, err := db.QueryContext(ctx, "SELECT p.configuration_json,p.scheduling_rank,p.model_selection_json FROM projects p JOIN token_grants g ON g.organization_id=p.organization_id AND g.project_id=p.id WHERE g.token_id=? AND p.organization_id=? ORDER BY p.scheduling_rank,p.created_at,p.id", LocalRunner, LocalOrganization)
	if err != nil {
		return cfg, revision, err
	}
	defer rows.Close()
	cfg.Projects = []globalconfig.Project{}
	for rows.Next() {
		var raw string
		var rank int
		var override sql.NullString
		if err := rows.Scan(&raw, &rank, &override); err != nil {
			return cfg, revision, err
		}
		var project globalconfig.Project
		if err := json.Unmarshal([]byte(raw), &project); err != nil {
			return cfg, revision, err
		}
		project.Weight, project.Priority = 1, rank
		var model config.ModelSelection
		if override.Valid {
			if err := json.Unmarshal([]byte(override.String), &model); err != nil {
				return cfg, revision, err
			}
		}
		project.ModelSelection = new(config.ResolveCloudModelSelection(*selection.Selection, model))
		cfg.Projects = append(cfg.Projects, project)
	}
	return cfg, revision, rows.Err()
}

func (s *sqliteStore) LocalConfiguration(ctx context.Context, cfg globalconfig.Config) (globalconfig.Config, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return cfg, err
	}
	defer tx.Rollback()
	value, _, err := localConfiguration(ctx, tx, cfg)
	return value, err
}

func (s *sqliteStore) MutateLocalConfiguration(ctx context.Context, cfg globalconfig.Config, mutate func(*globalconfig.Config, string) bool) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	value, revision, err := localConfiguration(ctx, tx, cfg)
	rollbackErr := tx.Rollback()
	if err != nil {
		return err
	}
	if rollbackErr != nil {
		return rollbackErr
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	if !mutate(&value, hex.EncodeToString(digest[:])) {
		return nil
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE organizations SET configuration_revision=configuration_revision+1 WHERE id=? AND configuration_revision=?", LocalOrganization, revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return projectsettings.ErrConflict
	}
	var ids []tracker.ProjectID
	seen := map[string]bool{}
	for _, project := range value.Projects {
		if project.ID == "" || seen[project.ID] {
			return projectsettings.ErrInvalid
		}
		seen[project.ID] = true
		ids = append(ids, tracker.ProjectID(project.ID))
		raw, err := json.Marshal(project)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO projects(id,organization_id,configuration_json,scheduling_rank,created_at) VALUES (?,?,?,(SELECT COALESCE(MAX(scheduling_rank),-1)+1 FROM projects WHERE organization_id=?),?) ON CONFLICT(id) DO UPDATE SET configuration_json=excluded.configuration_json", project.ID, LocalOrganization, string(raw), LocalOrganization, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if err := replaceAllowedProjects(ctx, tx, ids); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceAllowedProjects(ctx context.Context, tx *sql.Tx, ids []tracker.ProjectID) error {
	seen := map[tracker.ProjectID]bool{}
	for _, id := range ids {
		var exists int
		if seen[id] {
			return projectsettings.ErrInvalid
		}
		seen[id] = true
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM projects WHERE organization_id=? AND id=?", LocalOrganization, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return projectsettings.ErrInvalid
			}
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM token_grants WHERE token_id=? AND organization_id=?", LocalRunner, LocalOrganization); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,?,?)", LocalRunner, LocalOrganization, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteStore) LocalProjectRank(ctx context.Context, change *projectsettings.RankChange) (projectsettings.Rank, error) {
	if change == nil {
		return projectsettings.ReadRank(ctx, s.db, LocalOrganization)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return projectsettings.Rank{}, err
	}
	defer tx.Rollback()
	value, err := projectsettings.UpdateRank(ctx, tx, LocalOrganization, *change)
	if err != nil {
		return value, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE organizations SET configuration_revision=configuration_revision+1 WHERE id=?", LocalOrganization); err != nil {
		return value, err
	}
	return value, tx.Commit()
}

func (s *sqliteStore) LocalModelSelection(ctx context.Context, project tracker.ProjectID, change *projectsettings.ModelSelectionChange) (projectsettings.ModelSelection, error) {
	if change == nil {
		return projectsettings.ReadModelSelection(ctx, s.db, LocalOrganization, project)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return projectsettings.ModelSelection{}, err
	}
	defer tx.Rollback()
	value, err := projectsettings.UpdateModelSelection(ctx, tx, LocalOrganization, project, *change)
	if err != nil {
		return value, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE organizations SET configuration_revision=configuration_revision+1 WHERE id=?", LocalOrganization); err != nil {
		return value, err
	}
	return value, tx.Commit()
}

func (s *sqliteStore) LocalAllowedProjects(ctx context.Context, change *AllowedProjectsChange) (AllowedProjects, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AllowedProjects{}, err
	}
	defer tx.Rollback()
	var value AllowedProjects
	if err := tx.QueryRowContext(ctx, "SELECT configuration_revision FROM organizations WHERE id=?", LocalOrganization).Scan(&value.Revision); err != nil {
		return value, err
	}
	if change != nil {
		if change.ExpectedRevision != value.Revision {
			return value, projectsettings.ErrConflict
		}
		if err := replaceAllowedProjects(ctx, tx, change.ProjectIDs); err != nil {
			return value, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE organizations SET configuration_revision=configuration_revision+1 WHERE id=?", LocalOrganization); err != nil {
			return value, err
		}
		value.Revision++
	}
	rows, err := tx.QueryContext(ctx, "SELECT p.id FROM projects p JOIN token_grants g ON g.project_id=p.id AND g.organization_id=p.organization_id WHERE g.token_id=? AND p.organization_id=? ORDER BY p.scheduling_rank,p.created_at,p.id", LocalRunner, LocalOrganization)
	if err != nil {
		return value, err
	}
	value.ProjectIDs = []tracker.ProjectID{}
	for rows.Next() {
		var id tracker.ProjectID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return value, err
		}
		value.ProjectIDs = append(value.ProjectIDs, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return value, fmt.Errorf("read local allowed projects: %w", err)
	}
	return value, tx.Commit()
}
