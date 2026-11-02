package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreProjectImportSource struct {
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Objects        attachment.DetailStore
}

type coreProjectImportRow struct {
	Record          projectstore.Record
	Workflow        string
	Models          string
	States          string
	Created         string
	Deleted         sql.NullString
	RepositoryID    sql.NullInt64
	IconKind        sql.NullString
	IconName        sql.NullString
	IconColor       sql.NullString
	IconEmoji       sql.NullString
	IconImageKey    sql.NullString
	IconImageSHA256 sql.NullString
}

func (r *Repository) ImportCoreProjects(ctx context.Context, source CoreProjectImportSource) (int64, error) {
	if source.CopyPath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, errors.New("invalid core project import source")
	}
	db, copyHash, err := openCoreImportCopy(ctx, source.CopyPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	content := eventstore.NewContent(source.Objects)
	pin, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID, ActorID: "production-import", Operation: "core-project-source", CommandKey: "source", RequestHash: copyHash}, json.RawMessage(`{}`))
	if err != nil {
		return 0, err
	}
	if err := r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
		previous, err := s.PutReceipt(ctx, pin)
		if err != nil {
			return err
		}
		if previous.RequestHash != pin.RequestHash {
			return mutation.ErrConflict
		}
		return nil
	}); err != nil {
		return 0, err
	}
	var count int64
	var after string
	for {
		rows, err := readCoreProjectImportPage(ctx, tx, source.OrganizationID, after)
		if err != nil {
			return count, err
		}
		if len(rows) == 0 {
			verified, finalHash, err := openCoreImportCopy(ctx, source.CopyPath)
			if err != nil {
				return count, err
			}
			if err := verified.Close(); err != nil {
				return count, err
			}
			if finalHash != copyHash {
				return count, mutation.ErrConflict
			}
			return count, nil
		}
		for _, row := range rows {
			record, receipt, err := prepareCoreProjectImport(ctx, content, source, row)
			if err != nil {
				return count, err
			}
			err = r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				previous, err := s.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
				if err == nil {
					if previous.RequestHash != receipt.RequestHash {
						return mutation.ErrConflict
					}
					return nil
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if _, err := s.CreateProjectRecord(ctx, record); err != nil {
					return err
				}
				if _, err := s.db.ExecContext(ctx, `UPDATE projects SET created_at=$3,deleted_at=$4 WHERE organization_id=$1 AND id=$2`, source.OrganizationID, record.Core.ID, record.Core.CreatedAt, record.Core.DeletedAt); err != nil {
					return err
				}
				v := record.Settings
				if _, err := s.db.ExecContext(ctx, `UPDATE project_settings SET revision=$3,intake=$4,projection=$5,repository_enabled=$6,repository=$7,checkout_repository=$8,aging_enabled=$9,duplicate_challenge=$10,archive_completed_after_days=$11,archive_cancelled_after_days=$12,workflow_key=$13,workflow_sha256=$14,workflow_bytes=$15,workflow_content_bytes=$16,require_visual_attachments=$17,model_revision=$18,model_key=$19,model_sha256=$20,model_bytes=$21,model_content_bytes=$22 WHERE organization_id=$1 AND project_id=$2`, source.OrganizationID, record.Core.ID, v.Revision, v.Intake, v.Projection, v.RepositoryEnabled, v.Repository, v.CheckoutRepository, v.AgingEnabled, v.DuplicateChallenge, v.ArchiveCompletedAfterDays, v.ArchiveCancelledAfterDays, v.WorkflowKey, v.WorkflowSHA256, v.WorkflowBytes, v.WorkflowContentBytes, v.RequireVisualAttachments, v.ModelRevision, v.ModelKey, v.ModelSHA256, v.ModelBytes, v.ModelContentBytes); err != nil {
					return err
				}
				_, err = s.PutReceipt(ctx, receipt)
				return err
			})
			if err != nil {
				return count, fmt.Errorf("import core project: %w", err)
			}
			after = string(record.Core.ID)
			count++
		}
	}
}

func readCoreProjectImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, after string) ([]coreProjectImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.name,p.slug,p.profile,p.require_dependencies,p.scheduling_rank,p.content_settings_revision,p.workflow_source,p.workflow_source_revision,p.workflow_markdown,p.states_json,COALESCE(p.model_selection_json,''),p.created_at,p.deleted_at,p.integration_revision,p.github_intake,p.github_projection,p.github_repository_enabled,COALESCE(r.github_owner || '/' || r.github_name,''),p.checkout_repository,p.aging_enabled,p.duplicate_challenge,p.archive_completed_after_days,p.archive_cancelled_after_days,p.require_visual_attachments,p.model_selection_revision,p.repository_id,p.icon_kind,p.icon_name,p.icon_color,p.icon_emoji,p.icon_image_key,p.icon_image_sha256 FROM projects p LEFT JOIN repositories r ON r.id=p.repository_id WHERE p.organization_id=? AND p.id>? ORDER BY p.id LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreProjectImportRow, 0, 100)
	for rows.Next() {
		var row coreProjectImportRow
		c, v := &row.Record.Core, &row.Record.Settings
		c.OrganizationID = organization
		v.OrganizationID = organization
		err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.Profile, &c.RequireDependencies, &c.SchedulingRank, &c.ContentSettingsRevision, &c.WorkflowSource, &c.WorkflowSourceRevision, &row.Workflow, &row.States, &row.Models, &row.Created, &row.Deleted, &v.Revision, &v.Intake, &v.Projection, &v.RepositoryEnabled, &v.Repository, &v.CheckoutRepository, &v.AgingEnabled, &v.DuplicateChallenge, &v.ArchiveCompletedAfterDays, &v.ArchiveCancelledAfterDays, &v.RequireVisualAttachments, &v.ModelRevision, &row.RepositoryID, &row.IconKind, &row.IconName, &row.IconColor, &row.IconEmoji, &row.IconImageKey, &row.IconImageSHA256)
		if err != nil {
			return nil, err
		}
		v.ProjectID = c.ID
		result = append(result, row)
	}
	return result, rows.Err()
}

func prepareCoreProjectImport(ctx context.Context, content *eventstore.Content, source CoreProjectImportSource, row coreProjectImportRow) (projectstore.Record, eventstore.Receipt, error) {
	record := row.Record
	if record.Core.OrganizationID != source.OrganizationID || record.Settings.OrganizationID != source.OrganizationID || record.Core.ID == "" {
		return record, eventstore.Receipt{}, corestore.ErrTenantMismatch
	}
	if row.RepositoryID.Valid && record.Settings.Repository == "" {
		return record, eventstore.Receipt{}, errors.New("missing imported project repository")
	}
	var err error
	record.Core.CreatedAt, err = time.Parse(time.RFC3339Nano, row.Created)
	if err != nil {
		return record, eventstore.Receipt{}, errors.New("invalid project creation timestamp")
	}
	if row.Deleted.Valid {
		deleted, err := time.Parse(time.RFC3339Nano, row.Deleted.String)
		if err != nil {
			return record, eventstore.Receipt{}, errors.New("invalid project deletion timestamp")
		}
		record.Core.DeletedAt = &deleted
	}
	if err := json.Unmarshal([]byte(row.States), &record.States); err != nil {
		return record, eventstore.Receipt{}, errors.New("invalid imported project states")
	}
	if err := policy.ValidateStates(record.States); err != nil {
		return record, eventstore.Receipt{}, err
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return record, eventstore.Receipt{}, err
	}
	digest := sha256.Sum256(encoded)
	receipt := eventstore.Receipt{OrganizationID: source.OrganizationID, ActorID: "production-import", Operation: "core-project", CommandKey: string(record.Core.ID), RequestHash: hex.EncodeToString(digest[:])}
	receipt, err = content.PrepareReceipt(ctx, receipt, encoded)
	if err != nil {
		return record, receipt, err
	}
	workflow, err := json.Marshal(row.Workflow)
	if err != nil {
		return record, receipt, err
	}
	workflowRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID}, workflow)
	if err != nil {
		return record, receipt, err
	}
	record.Settings.WorkflowKey, record.Settings.WorkflowSHA256, record.Settings.WorkflowBytes, record.Settings.WorkflowContentBytes = workflowRef.PayloadObjectKey, workflowRef.PayloadSHA256, workflowRef.PayloadBytes, workflowRef.PayloadContentBytes
	record.Core.WorkflowObjectKey = &record.Settings.WorkflowKey
	if row.Models != "" {
		modelRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID}, json.RawMessage(row.Models))
		if err != nil {
			return record, receipt, err
		}
		record.Settings.ModelKey, record.Settings.ModelSHA256, record.Settings.ModelBytes, record.Settings.ModelContentBytes = modelRef.PayloadObjectKey, modelRef.PayloadSHA256, modelRef.PayloadBytes, modelRef.PayloadContentBytes
		record.Core.ModelSelectionObjectKey = &record.Settings.ModelKey
	}
	statesRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID}, json.RawMessage(row.States))
	if err != nil {
		return record, receipt, err
	}
	record.Core.StatesObjectKey = &statesRef.PayloadObjectKey
	return record, receipt, nil
}

func openCoreImportCopy(ctx context.Context, path string) (database *sql.DB, digest string, resultErr error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if err := file.Close(); err != nil {
			if database != nil {
				err = errors.Join(err, database.Close())
				database = nil
			}
			resultErr = errors.Join(resultErr, err)
		}
	}()
	before, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0222 != 0 {
		return nil, "", errors.New("import requires a sealed read-only database copy")
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Stat(path + suffix)
		if err == nil && info.Size() != 0 {
			return nil, "", errors.New("import copy has uncheckpointed journal data")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
	}
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}
	}
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {"ro"}, "immutable": {"1"}}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, "", err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, "", errors.Join(err, db.Close())
	}
	after, err := os.Stat(path)
	if err != nil {
		return nil, "", errors.Join(err, db.Close())
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || after.Mode().Perm()&0222 != 0 {
		return nil, "", errors.Join(errors.New("import copy changed while opening"), db.Close())
	}
	return db, hex.EncodeToString(hash.Sum(nil)), nil
}
