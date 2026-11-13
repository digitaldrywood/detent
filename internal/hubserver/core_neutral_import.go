package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readCoreNeutralObject(ctx context.Context, source *neutralimport.Query, organization tracker.OrganizationID, objects attachment.DetailStore, key string) ([]byte, error) {
	_, raw, err := readCoreNeutralObjectWithReference(ctx, source, organization, objects, key)
	return raw, err
}

func readCoreNeutralObjectWithReference(ctx context.Context, source *neutralimport.Query, organization tracker.OrganizationID, objects attachment.DetailStore, key string) (attachment.ArchiveReference, []byte, error) {
	row, err := source.One(ctx, "change_content_objects", map[string]string{"organization_id": string(organization), "object_key": key}, []string{"object_key"})
	ref := hubcontent.ContentReference()
	if errors.Is(err, sql.ErrNoRows) {
		row, err = source.One(ctx, "attachments", map[string]string{"organization_id": string(organization), "object_key": key, "kind": "archive", "customer_owned": "0", "storage_binding_id": ""}, []string{"object_key"})
		if err != nil {
			return attachment.ArchiveReference{}, nil, err
		}
		err = row.Decode(map[string]any{"object_key": &ref.Key, "sha256": &ref.SHA256, "size": &ref.CompressedSize, "uncompressed_size": &ref.UncompressedSize})
	} else if err == nil {
		err = row.Decode(map[string]any{"object_key": &ref.Key, "object_sha256": &ref.SHA256, "object_bytes": &ref.CompressedSize, "content_bytes": &ref.UncompressedSize})
	}
	if err != nil {
		return attachment.ArchiveReference{}, nil, err
	}
	if err := ref.Validate(string(organization)); err != nil {
		return attachment.ArchiveReference{}, nil, err
	}
	raw, err := objects.ReadDetail(ctx, string(organization), ref)
	if err == nil && int64(len(raw)) == ref.UncompressedSize {
		return ref, raw, nil
	}
	if err == nil {
		err = artifact.ErrIntegrity
	}
	cell, ok := row.Values["pending_content"]
	if !ok || cell.Null || len(cell.Data) == 0 {
		return attachment.ArchiveReference{}, nil, err
	}
	pending, decodeErr := row.Bytes("pending_content")
	if decodeErr != nil {
		return attachment.ArchiveReference{}, nil, decodeErr
	}
	prepared, _, prepareErr := attachment.PrepareArchive(string(organization), ref.SchemaVersion, bytes.NewReader(pending))
	if prepareErr != nil || prepared != ref {
		return attachment.ArchiveReference{}, nil, errors.Join(artifact.ErrIntegrity, prepareErr)
	}
	return ref, pending, nil
}

func resolveCoreNeutralJSON(ctx context.Context, source *neutralimport.Query, organization tracker.OrganizationID, objects attachment.DetailStore, raw []byte) ([]byte, error) {
	if strings.HasPrefix(string(raw), "orgs/") {
		return readCoreNeutralObject(ctx, source, organization, objects, string(raw))
	}
	return hubcontent.ResolveStorageJSONWith(ctx, raw, func(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
		if org != string(organization) || ref.Validate(org) != nil {
			return nil, artifact.ErrIntegrity
		}
		return readCoreNeutralArchive(ctx, source, organization, objects, ref)
	})
}

func reconstructCoreNeutralVersion(ctx context.Context, source *neutralimport.Query, objects attachment.DetailStore, organization, project, item, record string, revision int64) ([]byte, error) {
	if revision < 1 {
		return nil, sql.ErrNoRows
	}
	rows, err := source.Rows(ctx, "collaboration_versions", map[string]string{"organization_id": organization, "project_id": project, "work_item_id": item, "record_id": record}, []string{"revision"}, []string{strconv.FormatInt(max(0, revision-neutralimport.VersionSnapshotInterval), 10)}, neutralimport.VersionSnapshotInterval)
	if err != nil {
		return nil, err
	}
	type versionRow struct {
		revision      int64
		encoding, raw string
	}
	versions := make([]versionRow, 0, len(rows))
	snapshot := -1
	for _, row := range rows {
		var value versionRow
		if err := row.Decode(map[string]any{"revision": &value.revision, "encoding": &value.encoding, "record_json": &value.raw}); err != nil {
			return nil, err
		}
		if value.revision > revision {
			break
		}
		versions = append(versions, value)
		if value.encoding == "snapshot" {
			snapshot = len(versions) - 1
		}
	}
	if snapshot < 0 || versions[len(versions)-1].revision != revision {
		return nil, sql.ErrNoRows
	}
	var raw []byte
	for index := snapshot; index < len(versions); index++ {
		value := versions[index]
		encoded, err := resolveCoreNeutralJSON(ctx, source, tracker.OrganizationID(organization), objects, []byte(value.raw))
		if err != nil {
			return nil, err
		}
		if index > snapshot && value.revision != versions[index-1].revision+1 {
			return nil, sql.ErrNoRows
		}
		switch value.encoding {
		case "snapshot":
			raw = encoded
		case "patch":
			raw, err = neutralimport.ApplyVersionPatch(raw, encoded)
			if err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("unknown history encoding")
		}
	}
	return raw, nil
}
