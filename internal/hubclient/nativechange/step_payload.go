package nativechange

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubclient/evidence"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) retainStep(ctx context.Context, receipt tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return receipt, fmt.Errorf("encode step receipt: %w", artifact.ErrInvalid)
	}
	if len(raw) <= tracker.NativeStepReceiptBytes {
		return receipt, nil
	}
	bounded := receipt
	bounded.Validator, bounded.Validation, bounded.SecurityAudit = nil, nil, nil
	bounded.PayloadArtifact, bounded.PayloadAttachment = nil, nil
	summary, err := json.Marshal(bounded)
	if err != nil || len(summary) > tracker.NativeStepReceiptBytes/2 {
		return receipt, fmt.Errorf("retain step receipt identity: %w", artifact.ErrInvalid)
	}
	a := &e.artifacts
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		ref, err := e.retainStepAttachment(ctx, raw)
		if err != nil {
			return receipt, err
		}
		bounded.PayloadAttachment = &ref
		return bounded, nil
	}
	reservation := a.reservation
	reservation.Kind = "log"
	reservation.Key = e.data.AttemptID + ":step:" + artifact.Digest(raw)
	ref, err := evidence.Upload(ctx, a.client, reservation, raw)
	if err != nil {
		return receipt, err
	}
	bounded.PayloadArtifact = &ref
	return bounded, nil
}

func (e *nativeExecution) restoreStep(ctx context.Context, receipt tracker.NativeStepReceipt) (tracker.NativeStepReceipt, error) {
	if receipt.PayloadAttachment != nil {
		raw, err := e.readStepAttachment(ctx, *receipt.PayloadAttachment)
		if err != nil {
			return tracker.NativeStepReceipt{}, err
		}
		return tracker.DecodeNativeStepPayload(raw, receipt)
	}
	ref := receipt.PayloadArtifact
	if ref == nil {
		return receipt, nil
	}
	client := e.claim.source.NativeClient()
	if ref.OrganizationID != string(client.OrganizationID()) || ref.ProjectID != string(client.ProjectID()) || ref.WorkItemID != string(e.claim.lease.WorkItemID) || ref.Kind != "log" || ref.State != "complete" || ref.Availability != "available" || ref.Bytes <= 0 || ref.Bytes > artifact.MaxArtifactBytes {
		return tracker.NativeStepReceipt{}, artifact.ErrIntegrity
	}
	grant, err := client.ArtifactGrant(ctx, e.claim.lease.WorkItemID, ref.ArtifactID, ref.Revision)
	if err != nil {
		return tracker.NativeStepReceipt{}, err
	}
	content, err := artifact.ReadPayload(ctx, grant, *ref)
	if err != nil {
		return tracker.NativeStepReceipt{}, err
	}
	return tracker.DecodeNativeStepPayload(content, receipt)
}

func (e *nativeExecution) retainStepAttachment(ctx context.Context, raw []byte) (tracker.NativeStepAttachment, error) {
	client := e.claim.source.NativeClient()
	key := e.data.AttemptID + ":step:" + artifact.Digest(raw)
	request := attachment.EvidenceRequest{Mutation: tracker.Mutation{IdempotencyKey: key, LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken}, WorkItemID: e.claim.lease.WorkItemID, AttemptID: e.data.AttemptID, Caption: "Execution step receipt"}
	encoded, err := json.Marshal(request)
	if err != nil {
		return tracker.NativeStepAttachment{}, artifact.ErrInvalid
	}
	headers := http.Header{"Idempotency-Key": {key}, "X-Detent-Evidence": {base64.RawURLEncoding.EncodeToString(encoded)}}
	metadata, err := client.UploadAttachmentAt(ctx, bytes.NewReader(raw), "execution-step.json", "application/json", headers, "/attempts/"+e.data.AttemptID+"/evidence")
	if err != nil {
		return tracker.NativeStepAttachment{}, err
	}
	if metadata.SHA256 != artifact.Digest(raw) || metadata.Size != int64(len(raw)) {
		return tracker.NativeStepAttachment{}, artifact.ErrIntegrity
	}
	return tracker.NativeStepAttachment{ID: metadata.ID, OrganizationID: client.OrganizationID(), ProjectID: client.ProjectID(), WorkItemID: e.claim.lease.WorkItemID, SHA256: metadata.SHA256, Size: metadata.Size}, nil
}

func (e *nativeExecution) readStepAttachment(ctx context.Context, ref tracker.NativeStepAttachment) ([]byte, error) {
	client := e.claim.source.NativeClient()
	if !artifact.ValidID(ref.ID, "att") || ref.OrganizationID != client.OrganizationID() || ref.ProjectID != client.ProjectID() || ref.WorkItemID != e.claim.lease.WorkItemID || !artifact.ValidHash(ref.SHA256, 64) || ref.Size <= 0 || ref.Size > attachment.MaxBytes {
		return nil, artifact.ErrIntegrity
	}
	raw, _, err := client.Transport().Download(ctx, client.AttachmentPath("/attachments/"+ref.ID), ref.Size)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.Size || artifact.Digest(raw) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	return raw, nil
}
