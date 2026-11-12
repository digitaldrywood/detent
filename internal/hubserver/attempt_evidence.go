package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
			return nativePublicInvalid("Evidence requires its attempt endpoint")
		}
		return nil
	}
	if input == nil || input.AttemptID != c.Param("attempt") || strings.TrimSpace(input.Caption) == "" || len(input.Caption) > 1024 || !utf8.ValidString(input.Caption) || strings.ContainsAny(input.Caption, "\r\n\x00") || input.IdempotencyKey == "" || input.LeaseID == "" || input.FencingToken <= 0 {
		return nativePublicInvalid("Invalid attempt evidence")
	}
	return s.checkAttachmentEvidenceTenant(c.Request().Context(), nativeRequestScope(c), *input)
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
