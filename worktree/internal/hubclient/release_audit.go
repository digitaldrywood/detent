package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseAuditRecords struct{ client *NativeClient }

func (c *NativeClient) ReleaseAuditRecords() releaseaudit.Records {
	return &releaseAuditRecords{client: c}
}

func (r *releaseAuditRecords) Publish(ctx context.Context, snapshot releaseaudit.Snapshot) error {
	if snapshot.Identity.ProjectID != string(r.client.project) {
		return errors.New("release audit publication belongs to another project")
	}
	raw, checkpoint, err := releaseaudit.SnapshotBytes(snapshot)
	if err != nil {
		return err
	}
	var previous releaseaudit.Checkpoint
	readErr := r.client.client.request(ctx, http.MethodGet, r.client.base()+"/release-audits/"+checkpoint.ID, nil, &previous)
	if readErr == nil {
		checkpoint.Parts = previous.Parts
		if !previous.Valid() || !reflect.DeepEqual(previous, checkpoint) {
			return errors.New("release audit checkpoint already has different source or evidence")
		}
		return nil
	}
	var apiError *APIError
	if !errors.As(readErr, &apiError) || apiError.Status != http.StatusNotFound {
		return readErr
	}
	parts, err := releaseaudit.SplitDocument(raw)
	if err != nil {
		return err
	}
	offset := 0
	for index, part := range parts {
		name := fmt.Sprintf("release-audit-%s-part-%04d.txt", checkpoint.ID, index)
		upload, err := r.client.UploadAttachment(ctx, bytes.NewReader(part), name, "text/plain")
		if err != nil {
			return fmt.Errorf("upload release audit evidence: %w", err)
		}
		digest := sha256.Sum256(part)
		checkpoint.Parts = append(checkpoint.Parts, releaseaudit.Part{AttachmentID: upload.ID, Index: index, Offset: offset, Bytes: len(part), Digest: hex.EncodeToString(digest[:])})
		offset += len(part)
	}
	if !checkpoint.Valid() {
		return errors.New("release audit checkpoint is invalid")
	}
	request := struct {
		tracker.Mutation
		releaseaudit.Checkpoint
	}{Mutation: tracker.Mutation{IdempotencyKey: "release-audit-" + checkpoint.ID}, Checkpoint: checkpoint}
	var retained releaseaudit.Checkpoint
	if err := r.client.client.request(ctx, http.MethodPost, r.client.base()+"/release-audits", request, &retained); err != nil {
		return err
	}
	if !reflect.DeepEqual(retained, checkpoint) {
		return errors.New("release audit publication returned a different checkpoint")
	}
	return nil
}

func (r *releaseAuditRecords) Load(ctx context.Context, project string) ([]releaseaudit.Snapshot, error) {
	if project != string(r.client.project) {
		return nil, errors.New("release audit history belongs to another project")
	}
	var snapshots []releaseaudit.Snapshot
	cursor := ""
	for {
		var page releaseaudit.CheckpointPage
		if err := r.client.client.request(ctx, http.MethodGet, r.client.base()+"/release-audits?cursor="+url.QueryEscape(cursor), nil, &page); err != nil {
			return nil, err
		}
		if page.Items == nil {
			return nil, errors.New("release audit history is unavailable")
		}
		for _, checkpoint := range page.Items {
			if !checkpoint.Valid() || checkpoint.Identity.ProjectID != project {
				return nil, errors.New("release audit history checkpoint is invalid")
			}
			var raw bytes.Buffer
			for _, part := range checkpoint.Parts {
				result, err := r.client.readAgentAttachment(ctx, operatortool.ReadAttachment, operatortool.AttachmentOperationArguments{ProjectID: project, AttachmentID: part.AttachmentID, Offset: 0, Length: part.Bytes})
				if err != nil {
					return nil, err
				}
				content, ok := result.(operatortool.AttachmentContentResult)
				if !ok {
					return nil, errors.New("release audit evidence is unavailable")
				}
				decoded, err := base64.StdEncoding.Strict().DecodeString(content.ContentBase64)
				if err != nil {
					return nil, err
				}
				digest := sha256.Sum256(decoded)
				if len(decoded) != part.Bytes || raw.Len() != part.Offset || hex.EncodeToString(digest[:]) != part.Digest {
					return nil, errors.New("release audit evidence part failed integrity verification")
				}
				raw.Write(decoded)
			}
			digest := sha256.Sum256(raw.Bytes())
			if raw.Len() != checkpoint.Bytes || hex.EncodeToString(digest[:]) != checkpoint.Digest {
				return nil, errors.New("release audit evidence document failed integrity verification")
			}
			var snapshot releaseaudit.Snapshot
			if err := json.Unmarshal(raw.Bytes(), &snapshot); err != nil {
				return nil, err
			}
			_, retained, err := releaseaudit.SnapshotBytes(snapshot)
			if err != nil {
				return nil, err
			}
			retained.Parts = checkpoint.Parts
			if !reflect.DeepEqual(retained, checkpoint) {
				return nil, errors.New("release audit evidence does not match its checkpoint")
			}
			snapshots = append(snapshots, snapshot)
		}
		if page.Next == "" {
			return snapshots, nil
		}
		if page.Next <= cursor {
			return nil, errors.New("release audit history pagination did not advance")
		}
		cursor = page.Next
	}
}
