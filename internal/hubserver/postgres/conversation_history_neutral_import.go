package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"strconv"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func validateNeutralConversationHistory(ctx context.Context, source ConversationHistoryImportSource) error {
	for _, table := range conversationImportTablesInOrder {
		if err := source.neutral.ValidateCatalogTable(source.Catalog, source.Schema, table.source, table.keys); err != nil {
			return err
		}
	}
	for _, table := range []struct {
		name string
		keys []string
	}{{"conversation_attachment_blobs", []string{"artifact_ref"}}, {"attachments", []string{"id"}}} {
		if err := source.neutral.ValidateCatalogTable(source.Catalog, source.Schema, table.name, table.keys); err != nil {
			return err
		}
	}
	for _, table := range conversationImportTablesInOrder {
		var after int64
		for {
			rows, err := readNeutralConversationHistoryPage(ctx, source, table, after)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			after = rows[len(rows)-1].cursor
		}
	}
	return nil
}
func readNeutralConversationHistoryPage(ctx context.Context, source ConversationHistoryImportSource, table conversationImportTable, after int64) ([]conversationSourceRow, error) {
	rows, err := source.neutral.OrdinalPage(ctx, table.source, after, conversationImportPageSize)
	if err != nil {
		return nil, err
	}
	result := make([]conversationSourceRow, 0, len(rows))
	for _, raw := range rows {
		if raw.SourceRowID < 1 {
			return nil, storagecatalog.ErrValue
		}
		row := conversationSourceRow{rowID: raw.SourceRowID, cursor: raw.Ordinal, cells: raw.Values}
		if table.source == "conversation_events" {
			sequence, err := strconv.ParseInt(cellText(raw.Values["seq"]), 10, 64)
			if err != nil || sequence <= 0 {
				return nil, storagecatalog.ErrValue
			}
		}
		parent, err := neutralConversationParent(ctx, source, table.source, raw)
		if err != nil {
			return nil, err
		}
		row.project = cellText(parent.Values["project_id"])
		if table.source == "message_references" {
			if raw.SourcePosition == nil {
				return nil, storagecatalog.ErrValue
			}
			row.position = *raw.SourcePosition
		}
		result = append(result, row)
	}
	return result, nil
}
func neutralConversationParent(ctx context.Context, source ConversationHistoryImportSource, table string, row NeutralImportRow) (NeutralImportRow, error) {
	var conversation string
	switch table {
	case "conversations":
		return row, nil
	case "conversation_turn_batches":
		start, err := source.neutral.Lookup(ctx, "conversation_starts", []string{cellText(row.Values["attempt_id"])})
		if err != nil {
			return NeutralImportRow{}, err
		}
		conversation = cellText(start.Values["conversation_id"])
	case "message_references":
		message, err := source.neutral.Lookup(ctx, "conversation_messages", []string{cellText(row.Values["message_id"])})
		if err != nil {
			return NeutralImportRow{}, err
		}
		conversation = cellText(message.Values["conversation_id"])
		if conversation != cellText(row.Values["conversation_id"]) {
			return NeutralImportRow{}, storagecatalog.ErrOrganization
		}
	default:
		conversation = cellText(row.Values["conversation_id"])
	}
	if conversation == "" {
		return NeutralImportRow{}, storagecatalog.ErrValue
	}
	parent, err := source.neutral.Lookup(ctx, "conversations", []string{conversation})
	if err != nil {
		return NeutralImportRow{}, err
	}
	if cellText(parent.Values["organization_id"]) != source.Source.OrganizationID {
		return NeutralImportRow{}, storagecatalog.ErrOrganization
	}
	return parent, nil
}
func neutralConversationBlob(ctx context.Context, source ConversationHistoryImportSource, artifact string) ([]byte, error) {
	row, err := source.neutral.Lookup(ctx, "conversation_attachment_blobs", []string{artifact})
	if err != nil {
		return nil, err
	}
	cell := row.Values["content"]
	if cell.Null {
		return nil, storagecatalog.ErrValue
	}
	switch cell.Encoding {
	case "binary":
		return bytes.Clone(cell.Data), nil
	case "hex":
		data, err := hex.DecodeString(string(cell.Data))
		if err != nil {
			return nil, storagecatalog.ErrValue
		}
		return data, nil
	case "base64":
		data, err := base64.StdEncoding.Strict().DecodeString(string(cell.Data))
		if err != nil || base64.StdEncoding.EncodeToString(data) != string(cell.Data) {
			return nil, storagecatalog.ErrValue
		}
		return data, nil
	default:
		return nil, storagecatalog.ErrValue
	}
}
