package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func attachmentEvidenceCommand(scope nativeScope, operation string, input attachment.UploadRequest) (nativeCommandOptions, tracker.Mutation) {
	options := nativeCommandOptions{OperationID: operation + " " + string(scope.project), Feature: "collaboration"}
	command := tracker.Mutation{IdempotencyKey: input.RequestID}
	if input.Evidence != nil {
		options.Item, options.RequireLease = string(input.Evidence.WorkItemID), true
		command = input.Evidence.Mutation
	}
	return options, command
}

func (s *Service) checkAttachmentEvidence(c echo.Context, input *attachment.EvidenceRequest) (resultErr error) {
	if c.Param("attempt") == "" {
		if input != nil {
			return nativeInvalid("Evidence requires its attempt endpoint")
		}
		return nil
	}
	if input == nil || input.AttemptID != c.Param("attempt") || strings.TrimSpace(input.Caption) == "" || len(input.Caption) > 1024 || !utf8.ValidString(input.Caption) || strings.ContainsAny(input.Caption, "\r\n\x00") || input.IdempotencyKey == "" || input.LeaseID == "" || input.FencingToken <= 0 {
		return nativeInvalid("Invalid attempt evidence")
	}
	tx, err := s.database.db.BeginTx(c.Request().Context(), nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	now, err := s.database.currentTime()
	if err != nil {
		return err
	}
	_, err = readEvidenceAttempt(c.Request().Context(), tx, nativeRequestScope(c), *input, now)
	return err
}

func readEvidenceAttempt(ctx context.Context, tx *sql.Tx, scope nativeScope, input attachment.EvidenceRequest, now time.Time) (tracker.NativeRunData, error) {
	var data tracker.NativeRunData
	if err := requireNativeMutationLease(ctx, tx, scope, string(input.WorkItemID), input.Mutation, now); err != nil {
		return data, nativeStaleLease(err, "Evidence producer lease is no longer current")
	}
	var encoded string
	err := tx.QueryRowContext(ctx, `SELECT data_json FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND id=? AND lease_id=? AND fencing_token=? AND status='running'`, scope.organization, scope.project, input.WorkItemID, input.AttemptID, input.LeaseID, input.FencingToken).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return data, nativeStaleExecution("Evidence producer is not the running attempt")
	}
	if err != nil {
		return data, err
	}
	if err := json.Unmarshal([]byte(encoded), &data); err != nil {
		return data, err
	}
	return data, nil
}

func recordAttachmentEvidence(ctx context.Context, tx *sql.Tx, scope nativeScope, input attachment.EvidenceRequest, record attachment.Metadata, now time.Time) error {
	if _, err := readEvidenceAttempt(ctx, tx, scope, input, now); err != nil {
		return err
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, string(input.WorkItemID))
	if err != nil {
		return err
	}
	reference := record.Markdown(string(scope.organization))
	comment, err := insertNativeComment(ctx, tx, scope, issue, escapeEvidenceCaption(input.Caption)+"\n\n"+reference, nil, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE attachments SET evidence_attempt_id=?, evidence_caption=?, work_item_id=?, comment_id=? WHERE organization_id=? AND project_id=? AND id=?`, input.AttemptID, input.Caption, input.WorkItemID, comment.ID, scope.organization, scope.project, record.ID)
	return err
}

func escapeEvidenceCaption(caption string) string {
	escape := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "#", "\\#", "!", "\\!")
	return escape.Replace(caption)
}

func readAttachmentEvidence(ctx context.Context, query nativeQueryer, scope nativeScope, item string, data *tracker.NativeRunData) error {
	rows, err := query.QueryContext(ctx, `SELECT id,name,content_type,evidence_caption FROM attachments WHERE organization_id=? AND project_id=? AND work_item_id=? AND evidence_attempt_id=? ORDER BY created_at,id`, scope.organization, scope.project, item, data.AttemptID)
	if err != nil {
		return err
	}
	defer rows.Close()
	budget := 1 << 20
	for rows.Next() {
		var record attachment.Metadata
		var caption string
		if err := rows.Scan(&record.ID, &record.Name, &record.ContentType, &caption); err != nil {
			return err
		}
		record.ProjectID = string(scope.project)
		file := tracker.NativeEvidence{AttachmentID: record.ID, Caption: caption, Reference: record.Markdown(string(scope.organization))}
		encoded, err := json.Marshal(file)
		if err != nil {
			return err
		}
		budget -= len(encoded) + 1
		if budget < 0 {
			return &nativeError{Code: "attempt_record_too_large", Message: "Attempt evidence exceeds the 1048576-byte read limit; read the work item comments for all references", status: http.StatusUnprocessableEntity, publicMessage: true}
		}
		data.Evidence = append(data.Evidence, file)
	}
	return rows.Err()
}

func publishAttemptEvidence(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, data tracker.NativeRunData, now time.Time) error {
	if len(data.Evidence) == 0 {
		if data.CompletionBody == "" {
			return nil
		}
		var uploaded bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM attachments WHERE organization_id=? AND project_id=? AND evidence_attempt_id=?)`, scope.organization, scope.project, data.AttemptID).Scan(&uploaded); err != nil {
			return err
		}
		if !uploaded {
			return nil
		}
	}
	var evidence strings.Builder
	for _, file := range data.Evidence {
		evidence.WriteString("\n\n")
		evidence.WriteString(escapeEvidenceCaption(file.Caption))
		evidence.WriteString("\n\n")
		evidence.WriteString(file.Reference)
	}
	body := data.CompletionBody
	limit := (64 << 10) - evidence.Len()
	if len(body) > limit {
		body = body[:limit]
		for !utf8.ValidString(body) {
			body = body[:len(body)-1]
		}
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, string(item))
	if err != nil {
		return err
	}
	for _, file := range data.Evidence {
		if _, err := tx.ExecContext(ctx, `UPDATE attachments SET work_item_id=NULL WHERE organization_id=? AND project_id=? AND id=? AND work_item_id=? AND comment_id IS NULL`, scope.organization, scope.project, file.AttachmentID, item); err != nil {
			return err
		}
	}
	_, err = insertNativeComment(ctx, tx, scope, issue, strings.TrimSpace(body+evidence.String()), nil, now)
	return err
}
