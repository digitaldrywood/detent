package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/digitaldrywood/detent/internal/visualreview"
)

func (s *sqliteStore) CreateVisualReviewCapture(ctx context.Context, c visualreview.Capture) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing visualreview.Capture
	var capturedAt string
	err = tx.QueryRowContext(ctx, `SELECT issue_id,repository,pr,head_sha,base_sha,captured_at,title,summary,coverage_notes,manifest_json FROM visual_review_captures WHERE project_id=? AND capture_id=?`, c.ProjectID, c.CaptureID).Scan(&existing.IssueID, &existing.Repository, &existing.PR, &existing.HeadSHA, &existing.BaseSHA, &capturedAt, &existing.Title, &existing.Summary, &existing.CoverageNotes, &existing.ManifestJSON)
	if err == nil {
		existing.ProjectID = c.ProjectID
		existing.CaptureID = c.CaptureID
		existing.CapturedAt, err = time.Parse(time.RFC3339Nano, capturedAt)
		if err != nil {
			return fmt.Errorf("parse existing visual review capture time: %w", err)
		}
		existing.Assets, err = visualReviewAssets(ctx, tx, c.ProjectID, c.CaptureID)
		if err != nil {
			return err
		}
		if sameVisualReviewCapture(existing, c) {
			return nil
		}
		return visualreview.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	created := c.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO visual_review_captures VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.ProjectID, c.IssueID, c.Repository, c.PR, c.CaptureID, c.HeadSHA, c.BaseSHA, c.CapturedAt.UTC().Format(time.RFC3339Nano), c.Title, c.Summary, c.CoverageNotes, []byte(c.ManifestJSON), created.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert visual review capture: %w", err)
	}
	for _, a := range c.Assets {
		_, err = tx.ExecContext(ctx, `INSERT INTO visual_review_assets VALUES(?,?,?,?,?,?,?,?,?,?)`, c.ProjectID, c.CaptureID, a.ID, a.StorageKey, a.Kind, a.MediaType, a.SizeBytes, a.SHA256, a.Width, a.Height)
		if err != nil {
			return fmt.Errorf("insert visual review asset: %w", err)
		}
	}
	return tx.Commit()
}

type visualReviewQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func visualReviewAssets(ctx context.Context, db visualReviewQueryer, projectID, captureID string) ([]visualreview.Asset, error) {
	rows, err := db.QueryContext(ctx, `SELECT asset_id,storage_key,kind,media_type,size_bytes,sha256,width,height FROM visual_review_assets WHERE project_id=? AND capture_id=? ORDER BY asset_id`, projectID, captureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var assets []visualreview.Asset
	for rows.Next() {
		var a visualreview.Asset
		if err := rows.Scan(&a.ID, &a.StorageKey, &a.Kind, &a.MediaType, &a.SizeBytes, &a.SHA256, &a.Width, &a.Height); err != nil {
			return nil, err
		}
		assets = append(assets, a)
	}
	return assets, rows.Err()
}

func sameVisualReviewCapture(existing, candidate visualreview.Capture) bool {
	if existing.ProjectID != candidate.ProjectID || existing.IssueID != candidate.IssueID ||
		existing.Repository != candidate.Repository || existing.PR != candidate.PR ||
		existing.CaptureID != candidate.CaptureID || existing.HeadSHA != candidate.HeadSHA ||
		existing.BaseSHA != candidate.BaseSHA || !existing.CapturedAt.Equal(candidate.CapturedAt.UTC()) ||
		existing.Title != candidate.Title || existing.Summary != candidate.Summary ||
		existing.CoverageNotes != candidate.CoverageNotes || !bytes.Equal(existing.ManifestJSON, candidate.ManifestJSON) ||
		len(existing.Assets) != len(candidate.Assets) {
		return false
	}
	candidateAssets := make(map[string]visualreview.Asset, len(candidate.Assets))
	for _, asset := range candidate.Assets {
		candidateAssets[asset.ID] = asset
	}
	for _, asset := range existing.Assets {
		candidateAsset, ok := candidateAssets[asset.ID]
		if !ok || asset.StorageKey != candidateAsset.StorageKey || asset.Kind != candidateAsset.Kind ||
			asset.MediaType != candidateAsset.MediaType || asset.SizeBytes != candidateAsset.SizeBytes ||
			asset.SHA256 != candidateAsset.SHA256 || asset.Width != candidateAsset.Width || asset.Height != candidateAsset.Height {
			return false
		}
	}
	return true
}

func (s *sqliteStore) VisualReviewCapture(ctx context.Context, p, c string) (visualreview.Capture, error) {
	return s.visualReviewCapture(ctx, `WHERE c.project_id=? AND c.capture_id=?`, p, c)
}
func (s *sqliteStore) LatestVisualReviewCapture(ctx context.Context, p, i string) (visualreview.Capture, error) {
	return s.visualReviewCapture(ctx, `WHERE c.project_id=? AND c.issue_id=? ORDER BY c.rowid DESC LIMIT 1`, p, i)
}
func (s *sqliteStore) visualReviewCapture(ctx context.Context, clause string, args ...any) (visualreview.Capture, error) {
	var c visualreview.Capture
	var captured, created string
	err := s.db.QueryRowContext(ctx, `SELECT c.project_id,c.issue_id,c.repository,c.pr,c.capture_id,c.head_sha,c.base_sha,c.captured_at,c.title,c.summary,c.coverage_notes,c.manifest_json,c.created_at FROM visual_review_captures c `+clause, args...).Scan(&c.ProjectID, &c.IssueID, &c.Repository, &c.PR, &c.CaptureID, &c.HeadSHA, &c.BaseSHA, &captured, &c.Title, &c.Summary, &c.CoverageNotes, &c.ManifestJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return c, visualreview.ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.CapturedAt, err = time.Parse(time.RFC3339Nano, captured)
	if err != nil {
		return c, fmt.Errorf("parse visual review capture time: %w", err)
	}
	c.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return c, fmt.Errorf("parse visual review creation time: %w", err)
	}
	c.Assets, err = visualReviewAssets(ctx, s.db, c.ProjectID, c.CaptureID)
	if err != nil {
		return c, err
	}
	return c, nil
}
func (s *sqliteStore) ListVisualReviewCaptures(ctx context.Context, p, i string) ([]visualreview.Capture, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT capture_id FROM visual_review_captures WHERE project_id=? AND issue_id=? ORDER BY rowid DESC`, p, i)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	var out []visualreview.Capture
	for _, id := range ids {
		c, err := s.VisualReviewCapture(ctx, p, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) VisualReviewDraft(ctx context.Context, p, c string) (visualreview.Draft, error) {
	var d visualreview.Draft
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT d.project_id,d.capture_id,d.head_sha,d.revision,d.feedback_json,d.audit_actor,d.updated_at FROM visual_review_drafts d WHERE d.project_id=? AND d.capture_id=?`, p, c).Scan(&d.ProjectID, &d.CaptureID, &d.HeadSHA, &d.Revision, &d.FeedbackJSON, &d.AuditActor, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		cap, e := s.VisualReviewCapture(ctx, p, c)
		if e != nil {
			return d, e
		}
		return visualreview.Draft{ProjectID: p, CaptureID: c, HeadSHA: cap.HeadSHA}, nil
	}
	if err != nil {
		return d, err
	}
	d.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return d, fmt.Errorf("parse visual review draft update time: %w", err)
	}
	return d, nil
}
func (s *sqliteStore) SaveVisualReviewDraft(ctx context.Context, p, c, head string, rev int64, json []byte, actor string, now time.Time) (visualreview.Draft, error) {
	cap, err := s.VisualReviewCapture(ctx, p, c)
	if err != nil {
		return visualreview.Draft{}, err
	}
	if cap.HeadSHA != head {
		return visualreview.Draft{}, visualreview.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return visualreview.Draft{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE visual_review_drafts SET revision=revision+1,feedback_json=?,audit_actor=?,updated_at=? WHERE project_id=? AND capture_id=? AND head_sha=? AND revision=?`, json, actor, now.UTC().Format(time.RFC3339Nano), p, c, head, rev)
	if err != nil {
		return visualreview.Draft{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return visualreview.Draft{}, err
	}
	if n == 0 && rev == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO visual_review_drafts VALUES(?,?,?,?,?,?,?)`, p, c, head, 1, json, actor, now.UTC().Format(time.RFC3339Nano))
		if err != nil {
			if isVisualReviewUniquenessError(err) {
				return visualreview.Draft{}, visualreview.ErrConflict
			}
			return visualreview.Draft{}, fmt.Errorf("insert visual review draft: %w", err)
		}
	} else if n == 0 {
		return visualreview.Draft{}, visualreview.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return visualreview.Draft{}, err
	}
	return visualreview.Draft{ProjectID: p, CaptureID: c, HeadSHA: head, Revision: rev + 1, FeedbackJSON: append([]byte(nil), json...), AuditActor: actor, UpdatedAt: now.UTC()}, nil
}

func isVisualReviewUniquenessError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY ||
		sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
