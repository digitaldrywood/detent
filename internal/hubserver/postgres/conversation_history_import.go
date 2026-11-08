package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const conversationImportPageSize = 100

type ConversationHistoryImportSource struct {
	BundlePath string
	neutral    *VerifiedNeutralImport
	CopyPath   string
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	Objects    attachment.DetailStore
}

type ConversationHistoryImportResult struct {
	Rows int64
}

type conversationImportTable struct {
	source string
	target string
	keys   []string
}

type conversationSourceRow struct {
	cursor   int64
	rowID    int64
	project  string
	position int64
	cells    map[string]storagecatalog.Cell
}

type conversationArchiveSource struct {
	id        string
	project   tracker.ProjectID
	uploader  string
	reference attachment.ArchiveReference
	createdAt time.Time
}

var conversationImportTablesInOrder = []conversationImportTable{
	{source: "conversations", target: "conversations", keys: []string{"id"}},
	{source: "conversation_messages", target: "conversation_messages", keys: []string{"id"}},
	{source: "conversation_questions", target: "conversation_questions", keys: []string{"id"}},
	{source: "conversation_commands", target: "conversation_commands", keys: []string{"conversation_id", "key"}},
	{source: "conversation_events", target: "conversation_events", keys: []string{"conversation_id", "seq"}},
	{source: "conversation_starts", target: "conversation_starts", keys: []string{"attempt_id"}},
	{source: "conversation_audience_events", target: "conversation_audience_events", keys: []string{"id"}},
	{source: "conversation_turn_batches", target: "conversation_turn_batches", keys: []string{"attempt_id", "batch_key"}},
	{source: "conversation_tool_revisions", target: "conversation_tool_revisions", keys: []string{"conversation_id", "message_id", "revision"}},
	{source: "message_references", target: "message_references", keys: []string{"message_id", "target_kind", "target_id"}},
	{source: "conversation_attachments", target: "conversation_attachments", keys: []string{"id"}},
}

func (r *Repository) ImportConversationHistory(ctx context.Context, source ConversationHistoryImportSource) (ConversationHistoryImportResult, error) {
	if source.CopyPath != "" {
		return ConversationHistoryImportResult{},
			ErrNeutralImport
	}

	var result ConversationHistoryImportResult
	if source.BundlePath == "" || source.Objects == nil || source.Source.Store != "hub" || strings.TrimSpace(source.Source.OrganizationID) != source.Source.OrganizationID || source.Source.OrganizationID == "" {
		return result, errors.New("invalid conversation history import source")
	}
	if source.Schema == nil {
		source.Schema = schemaFromCatalog(source.Catalog)
	}
	source.Source.Schema = "hub_copy"
	if _, err := source.Catalog.Bind(source.Source, source.Schema); err != nil {
		return result, err
	}
	organization := tracker.OrganizationID(source.Source.OrganizationID)
	catalogDigest, err := conversationImportCatalogDigest(source.Catalog, source.Schema)
	if err != nil {
		return result, err
	}
	completed := false
	snapshotID := ""

	err = ReadNeutralImportBundle(ctx, source.BundlePath, organization, func(bundle *VerifiedNeutralImport) error {
		source.neutral = bundle
		snapshotID = bundle.Identity().SourceSHA256
		if err := validateNeutralConversationHistory(ctx, source); err != nil {
			return err
		}
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var err error
		completed, result.Rows, err = r.beginConversationHistoryImport(ctx, organization, source.Source.Schema, snapshotID, catalogDigest)
		if err != nil || completed {
			return err
		}
		return r.importConversationHistoryCopy(ctx, snapshotID, source, organization, &result)
	})

	if err == nil && !completed {
		err = r.completeConversationHistoryImport(ctx, organization, source.Source.Schema, snapshotID, catalogDigest, result.Rows)
	}
	return result, err
}

func conversationImportCatalogDigest(catalog storagecatalog.Catalog, schema storagecatalog.Schema) (string, error) {
	encoded, err := json.Marshal(struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}{catalog, schema})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (r *Repository) beginConversationHistoryImport(ctx context.Context, organization tracker.OrganizationID, sourceSchema, snapshotID, catalogDigest string) (bool, int64, error) {
	var completed bool
	var importedRows int64
	err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		if _, err := s.db.Exec(ctx, `INSERT INTO conversation_history_imports(organization_id,source_schema,snapshot_id,catalog_digest) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id) DO NOTHING`, organization, sourceSchema, snapshotID, catalogDigest); err != nil {
			return mapError(ctx, err)
		}
		var storedSchema, storedSnapshot, storedCatalog string
		if err := s.db.QueryRow(ctx, `SELECT source_schema,snapshot_id,catalog_digest,completed,rows FROM conversation_history_imports WHERE organization_id=$1`, organization).Scan(&storedSchema, &storedSnapshot, &storedCatalog, &completed, &importedRows); err != nil {
			return mapError(ctx, err)
		}
		if storedSchema != sourceSchema || storedSnapshot != snapshotID || storedCatalog != catalogDigest {
			return mutation.ErrConflict
		}
		return nil
	})
	return completed, importedRows, err
}

func (r *Repository) completeConversationHistoryImport(ctx context.Context, organization tracker.OrganizationID, sourceSchema, snapshotID, catalogDigest string, rows int64) error {
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		result, err := s.db.Exec(ctx, `UPDATE conversation_history_imports SET completed=true,rows=$5 WHERE organization_id=$1 AND source_schema=$2 AND snapshot_id=$3 AND catalog_digest=$4 AND (NOT completed OR rows=$5)`, organization, sourceSchema, snapshotID, catalogDigest, rows)
		if err != nil {
			return mapError(ctx, err)
		}
		count := result.RowsAffected()
		if count != 1 {
			return mutation.ErrConflict
		}
		return nil
	})
}

func (r *Repository) importConversationHistoryCopy(ctx context.Context, copyHash string, source ConversationHistoryImportSource, organization tracker.OrganizationID, result *ConversationHistoryImportResult) error {
	for _, table := range conversationImportTablesInOrder {
		var after int64
		for {
			page, err := readConversationHistoryPage(ctx, source, table, organization, after)
			if err != nil {
				return fmt.Errorf("read %s import page: %w", table.source, err)
			}
			if len(page) == 0 {
				break
			}
			batch := ConversationImportBatch{OrganizationID: organization}
			archiveIDs := make(map[string]conversationstore.ContentObject)
			for _, row := range page {
				prepared, objects, err := prepareConversationImportRow(ctx, source, copyHash, organization, table, row, archiveIDs)
				if err != nil {
					return fmt.Errorf("prepare %s row %d: %w", table.source, row.rowID, err)
				}
				batch.Rows = append(batch.Rows, prepared...)
				batch.ContentObjects = append(batch.ContentObjects, objects...)
				if table.source == "conversation_attachments" {
					attachment, object, err := prepareConversationAttachment(ctx, source, copyHash, organization, row, archiveIDs)
					if err != nil {
						return fmt.Errorf("prepare conversation attachment row %d: %w", row.rowID, err)
					}
					batch.Attachments = append(batch.Attachments, attachment)
					if object != nil {
						batch.ContentObjects = append(batch.ContentObjects, *object)
					}
				}

				after = row.cursor

			}
			batch.ContentObjects = uniqueConversationContentObjects(batch.ContentObjects)
			if err := r.ImportConversationBatch(ctx, batch); err != nil {
				return fmt.Errorf("write %s import page: %w", table.source, err)
			}
			result.Rows += int64(len(page))
		}
	}
	return nil
}

func readConversationHistoryPage(ctx context.Context, source ConversationHistoryImportSource, table conversationImportTable, organization tracker.OrganizationID, after int64) ([]conversationSourceRow, error) {
	return readNeutralConversationHistoryPage(ctx, source, table, after)
}

func prepareConversationImportRow(ctx context.Context, source ConversationHistoryImportSource, copyHash string, organization tracker.OrganizationID, table conversationImportTable, row conversationSourceRow, archiveIDs map[string]conversationstore.ContentObject) ([]ConversationImportRow, []conversationstore.ContentObject, error) {
	if table.source == "conversation_attachments" {
		return nil, nil, nil
	}
	binding, err := source.Catalog.Bind(source.Source, source.Schema)
	if err != nil {
		return nil, nil, err
	}
	if _, err := binding.ValidateMappedRow(table.source, row.cells); err != nil {
		return nil, nil, err
	}
	targetValues := map[string]any{"organization_id": organization}
	var contentID string
	if cell, ok := row.cells["content_attachment_id"]; ok && !cell.Null && cellText(cell) != "" {
		sourceID := cellText(cell)
		object, found := archiveIDs[sourceID]
		if !found {
			object, err = prepareConversationArchiveObject(ctx, source, organization, sourceID)
			if err != nil {
				return nil, nil, err
			}
			archiveIDs[sourceID] = object
		}
		contentID = object.ID
	}
	for name, cell := range row.cells {
		column := source.Catalog.Tables["hub/"+table.source].Columns[name]
		if column.Disposition == "rebuild" {
			if name == "conversion_state" {
				targetValues[name] = int16(2)
			}
			continue
		}
		if column.Disposition != "column" || len(column.Targets) != 1 || column.Targets[0].Table != "public."+table.target {
			return nil, nil, storagecatalog.ErrUnmapped
		}
		decoded, err := source.Catalog.Decode("hub/"+table.source, name, cell)
		if err != nil {
			return nil, nil, err
		}
		if name == "content_attachment_id" && contentID != "" {
			targetValues[column.Targets[0].Column] = contentID
			continue
		}
		targetValues[column.Targets[0].Column] = conversationImportValue(column.Codec, decoded)
	}
	if table.source == "message_references" {
		targetValues["position"] = row.position
	}
	columns, values := sortedConversationImportValues(targetValues)
	request, err := json.Marshal(struct {
		Copy    string
		Table   string
		Row     int64
		Columns []string
		Values  []any
	}{copyHash, table.target, row.rowID, columns, values})
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(request)
	createdAt := time.Unix(0, 0).UTC()
	if value, ok := targetValues["created_at"].(time.Time); ok {
		createdAt = value
	}
	receipt := ConversationImportReceipt{SnapshotID: copyHash, Table: table.target, SourceRow: row.rowID, RequestHash: hex.EncodeToString(digest[:]), CreatedAt: createdAt}
	return []ConversationImportRow{{Table: table.target, Columns: columns, Values: values, Receipt: receipt}}, conversationObjectsForCell(row.cells, archiveIDs), nil
}

func conversationImportValue(codec string, value storagecatalog.Value) any {
	if value.Null {
		return nil
	}
	switch codec {
	case "text":
		return value.Text
	case "json":
		return string(value.Bytes)
	case "int32":
		return int32(value.Integer)
	case "int64":
		return value.Integer
	case "bool01":
		return value.Boolean
	case "float64":
		return value.Real
	case "timestamp", "unix-seconds":
		return value.Time
	default:
		return nil
	}
}

func sortedConversationImportValues(values map[string]any) ([]string, []any) {
	columns := make([]string, 0, len(values))
	for column := range values {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	args := make([]any, len(columns))
	for i, column := range columns {
		args[i] = values[column]
	}
	return columns, args
}

func conversationObjectsForCell(cells map[string]storagecatalog.Cell, archiveIDs map[string]conversationstore.ContentObject) []conversationstore.ContentObject {
	cell, ok := cells["content_attachment_id"]
	if !ok || cell.Null || cellText(cell) == "" {
		return nil
	}
	object, ok := archiveIDs[cellText(cell)]
	if !ok {
		return nil
	}
	return []conversationstore.ContentObject{object}
}

func prepareConversationAttachment(ctx context.Context, source ConversationHistoryImportSource, copyHash string, organization tracker.OrganizationID, row conversationSourceRow, archiveIDs map[string]conversationstore.ContentObject) (ConversationImportAttachment, *conversationstore.ContentObject, error) {
	values := make(map[string]storagecatalog.Value, len(row.cells))
	for column, cell := range row.cells {
		if column == "conversion_state" {
			continue
		}
		decoded, err := source.Catalog.Decode("hub/conversation_attachments", column, cell)
		if err != nil {
			return ConversationImportAttachment{}, nil, err
		}
		values[column] = decoded
	}
	conversationID := values["conversation_id"].Text
	attachmentID := values["id"].Text
	artifactRef := values["artifact_ref"].Text
	contentID := ""
	var object *conversationstore.ContentObject
	if oldContentID := values["content_attachment_id"].Text; oldContentID != "" {
		prepared, found := archiveIDs[oldContentID]
		if !found {
			var err error
			prepared, err = prepareConversationArchiveObject(ctx, source, organization, oldContentID)
			if err != nil {
				return ConversationImportAttachment{}, nil, err
			}
			archiveIDs[oldContentID] = prepared
		}
		contentID, object = prepared.ID, &prepared
	} else if values["deleted_at"].Null {
		var blob []byte

		var err error
		blob, err = neutralConversationBlob(ctx, source, artifactRef)
		if err != nil {
			return ConversationImportAttachment{}, nil, err
		}

		if int64(len(blob)) != values["size"].Integer || len(blob) == 0 {
			return ConversationImportAttachment{}, nil, artifact.ErrIntegrity
		}
		ref, err := source.Objects.StoreDetail(ctx, string(organization), blob)
		if err != nil {
			return ConversationImportAttachment{}, nil, err
		}
		stored, err := source.Objects.ReadDetail(ctx, string(organization), ref)
		if err != nil || !bytes.Equal(stored, blob) {
			return ConversationImportAttachment{}, nil, errors.Join(artifact.ErrIntegrity, err)
		}
		contentID = pgConversationContentID(string(organization), ref)
		prepared := conversationstore.ContentObject{OrganizationID: organization, ID: contentID, Reference: ref, CreatedAt: values["created_at"].Time}
		object = &prepared
	}
	var messageID string
	if !values["message_id"].Null {
		messageID = values["message_id"].Text
	}
	var expiresAt, deletedAt *time.Time
	if !values["expires_at"].Null {
		expiresAt = new(values["expires_at"].Time)
	}
	if !values["deleted_at"].Null {
		deletedAt = new(values["deleted_at"].Time)
	}
	request, err := json.Marshal(struct {
		Copy    string
		Row     int64
		Content string
	}{copyHash, row.rowID, contentID})
	if err != nil {
		return ConversationImportAttachment{}, nil, err
	}
	digest := sha256.Sum256(request)
	createdAt := values["created_at"].Time
	record := conversationstore.Attachment{OrganizationID: organization, ConversationID: conversationID, ProjectID: tracker.ProjectID(row.project), ID: attachmentID, MessageID: messageID, PrincipalID: values["principal_id"].Text, Name: values["name"].Text, MIME: values["mime"].Text, Size: values["size"].Integer, ArtifactRef: artifactRef, ContentID: contentID, CreatedAt: createdAt, ExpiresAt: expiresAt, DeletedAt: deletedAt}
	receipt := ConversationImportReceipt{SnapshotID: copyHash, Table: "conversation_attachments", SourceRow: row.rowID, RequestHash: hex.EncodeToString(digest[:]), CreatedAt: createdAt}
	return ConversationImportAttachment{Record: record, Receipt: receipt}, object, nil
}

func prepareConversationArchiveObject(ctx context.Context, source ConversationHistoryImportSource, organization tracker.OrganizationID, sourceID string) (conversationstore.ContentObject, error) {
	var metadata conversationArchiveSource
	var kind, retention, createdAt string
	var compressed, uncompressed int64
	var schema int
	var binding string
	var customerOwned bool
	var err error

	var row NeutralImportRow
	row, err = source.neutral.Lookup(ctx, "attachments", []string{sourceID})
	if err == nil {
		err = row.Decode(map[string]any{"id": &metadata.id, "project_id": &metadata.project, "uploader": &metadata.uploader, "object_key": &metadata.reference.Key, "sha256": &metadata.reference.SHA256, "size": &compressed, "uncompressed_size": &uncompressed, "codec": &metadata.reference.Codec, "schema_version": &schema, "kind": &kind, "retention_class": &retention, "storage_binding_id": &binding, "customer_owned": &customerOwned, "created_at": &createdAt})
	}

	if err != nil {
		return conversationstore.ContentObject{}, err
	}
	metadata.reference.CompressedSize, metadata.reference.UncompressedSize = compressed, uncompressed
	metadata.reference.SchemaVersion, metadata.reference.Kind, metadata.reference.Retention = schema, kind, retention
	metadata.reference.BindingID, metadata.reference.CustomerOwned = binding, customerOwned
	if kind != "archive" || metadata.reference.Validate(string(organization)) != nil {
		return conversationstore.ContentObject{}, artifact.ErrIntegrity
	}
	metadata.createdAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return conversationstore.ContentObject{}, err
	}
	content, err := source.Objects.ReadDetail(ctx, string(organization), metadata.reference)
	if err != nil {
		return conversationstore.ContentObject{}, err
	}
	if int64(len(content)) != metadata.reference.UncompressedSize {
		return conversationstore.ContentObject{}, artifact.ErrIntegrity
	}
	id := pgConversationContentID(string(organization), metadata.reference)
	return conversationstore.ContentObject{OrganizationID: organization, ID: id, Reference: metadata.reference, CreatedAt: metadata.createdAt}, nil
}

func cellText(cell storagecatalog.Cell) string {
	if cell.Null {
		return ""
	}
	return string(cell.Data)
}

func uniqueConversationContentObjects(objects []conversationstore.ContentObject) []conversationstore.ContentObject {
	seen := make(map[string]struct{}, len(objects))
	result := make([]conversationstore.ContentObject, 0, len(objects))
	for _, object := range objects {
		if _, duplicate := seen[object.ID]; duplicate {
			continue
		}
		seen[object.ID] = struct{}{}
		result = append(result, object)
	}
	return result
}
