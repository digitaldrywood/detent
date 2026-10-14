package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	data, err := readEvidenceAttempt(ctx, tx, scope, input, now)
	if err != nil {
		return err
	}
	if len(data.Evidence) >= 10 {
		return nativeInvalid("An attempt may attach at most ten evidence files")
	}
	reference := record.Markdown(string(scope.organization))
	data.Evidence = append(data.Evidence, tracker.NativeEvidence{AttachmentID: record.ID, Caption: input.Caption, Reference: reference})
	encoded, err := marshalNative(data)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE native_attempts SET data_json=?, updated_at=? WHERE id=?", encoded, formatHubTime(now), input.AttemptID); err != nil {
		return err
	}
	return bindCloudAttachmentReferences(ctx, tx, scope, string(input.WorkItemID), "", reference)
}

func publishAttemptEvidence(ctx context.Context, tx *sql.Tx, scope nativeScope, item tracker.NativeWorkItemID, data tracker.NativeRunData, now time.Time) error {
	if len(data.Evidence) == 0 {
		return nil
	}
	var evidence strings.Builder
	escape := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "#", "\\#", "!", "\\!")
	for _, file := range data.Evidence {
		evidence.WriteString("\n\n")
		evidence.WriteString(escape.Replace(file.Caption))
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
