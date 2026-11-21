package hubrecords

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ScanAttachmentArchive(row interface{ Scan(...any) error }) (attachment.ArchiveReference, error) {
	var ref attachment.ArchiveReference
	err := row.Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned)
	return ref, err
}

type TenantAttemptSnapshot struct {
	ArtifactsPayload     *attachment.ArchiveReference `json:"-"`
	CheckpointPayload    *attachment.ArchiveReference `json:"-"`
	RecoverPayload       bool                         `json:"-"`
	Payload              *attachment.ArchiveReference `json:"-"`
	Attempt              leasestore.Attempt           `json:"-"`
	Lease                leasestore.Lease             `json:"-"`
	Event                eventstore.Event             `json:"-"`
	RunnerHeartbeatStale bool                         `json:"-"`
	EvidencePage         bool                         `json:"-"`
	Evidence             bool                         `json:"-"`
	EvidenceRows         []attachmentstore.Record     `json:"-"`
}

type NativeAttemptIdentityValues struct {
	Expected int64
	Actual   int64
}

type NativeAttemptIdentityMismatch struct {
	AttemptID  string
	WorkItemID tracker.NativeWorkItemID
	Field      string
	Values     *NativeAttemptIdentityValues
}

func (e *NativeAttemptIdentityMismatch) Error() string {
	return artifact.ErrIntegrity.Error() + ": " + e.Field
}

func (*NativeAttemptIdentityMismatch) Unwrap() error { return artifact.ErrIntegrity }

func NativeAttemptRunMismatch(snapshot TenantAttemptSnapshot, run tracker.NativeRunData) *NativeAttemptIdentityMismatch {
	attempt, lease := snapshot.Attempt, snapshot.Lease
	mismatch := func(field string, values *NativeAttemptIdentityValues) *NativeAttemptIdentityMismatch {
		return &NativeAttemptIdentityMismatch{AttemptID: attempt.ID, WorkItemID: snapshot.Event.WorkItemID, Field: field, Values: values}
	}
	switch {
	case run.AttemptID != attempt.ID:
		return mismatch("attempt_id", nil)
	case run.RunID != attempt.RunID:
		return mismatch("run_id", nil)
	case run.LeaseID != tracker.LeaseID(lease.LeaseID):
		return mismatch("lease_id", nil)
	case run.FencingToken != tracker.FencingToken(lease.FencingToken):
		return mismatch("fencing_token", &NativeAttemptIdentityValues{Expected: lease.FencingToken, Actual: int64(run.FencingToken)})
	case attempt.LocalAttemptID > 0 && run.Runtime != nil && run.Runtime.LocalAttemptID > 0 && run.Runtime.LocalAttemptID != attempt.LocalAttemptID:
		return mismatch("local_attempt_id", &NativeAttemptIdentityValues{Expected: attempt.LocalAttemptID, Actual: run.Runtime.LocalAttemptID})
	case run.Sequence == attempt.Sequence:
		return nil
	case attempt.Status != "cancelled" || attempt.Sequence != run.Sequence+1 || run.Outcome != "":
		return mismatch("sequence", &NativeAttemptIdentityValues{Expected: attempt.Sequence, Actual: run.Sequence})
	case lease.ReleasedAt == nil:
		return mismatch("released_at", nil)
	case !attempt.UpdatedAt.Equal(*lease.ReleasedAt):
		return mismatch("updated_at", &NativeAttemptIdentityValues{Expected: attempt.UpdatedAt.UnixMicro(), Actual: lease.ReleasedAt.UnixMicro()})
	default:
		return nil
	}
}

type AttemptArchiveReader interface {
	ArchiveObjectByKey(context.Context, string) (artifactstore.ArchiveObject, error)
}

func ReadTenantAttemptPayload(ctx context.Context, r tenantstore.Reader, snapshot *TenantAttemptSnapshot) error {
	if err := ReadTenantAttemptRunPayload(ctx, r, snapshot); err != nil {
		return err
	}
	if err := ReadTenantAttemptCheckpointPayload(ctx, r, snapshot); err != nil {
		return err
	}
	if snapshot.Attempt.ArtifactsObjectKey == nil {
		return nil
	}
	reader, ok := r.(AttemptArchiveReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	object, err := reader.ArchiveObjectByKey(ctx, *snapshot.Attempt.ArtifactsObjectKey)
	if errors.Is(err, corestore.ErrNotFound) {
		return artifact.ErrIntegrity
	}
	if err != nil {
		return err
	}
	if object.OrganizationID != snapshot.Attempt.OrganizationID || object.Reference.Key != *snapshot.Attempt.ArtifactsObjectKey || object.Reference.Validate(string(snapshot.Attempt.OrganizationID)) != nil {
		return artifact.ErrIntegrity
	}
	snapshot.ArtifactsPayload = &object.Reference
	return nil
}

func ReadTenantAttemptCheckpointPayload(ctx context.Context, r tenantstore.Reader, snapshot *TenantAttemptSnapshot) error {
	if snapshot.Attempt.CheckpointObjectKey == nil {
		return nil
	}
	reader, ok := r.(AttemptArchiveReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	object, err := reader.ArchiveObjectByKey(ctx, *snapshot.Attempt.CheckpointObjectKey)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if object.OrganizationID != snapshot.Attempt.OrganizationID || object.Reference.Key != *snapshot.Attempt.CheckpointObjectKey || object.Reference.Validate(string(snapshot.Attempt.OrganizationID)) != nil {
		return artifact.ErrIntegrity
	}
	snapshot.CheckpointPayload = &object.Reference
	return nil
}

func ReadTenantAttemptRunPayload(ctx context.Context, r tenantstore.Reader, snapshot *TenantAttemptSnapshot) error {
	reader, ok := r.(AttemptArchiveReader)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	object, err := reader.ArchiveObjectByKey(ctx, snapshot.Attempt.PayloadObjectKey)
	if errors.Is(err, corestore.ErrNotFound) {
		return ReadTenantAttemptAttachmentPayload(ctx, r, snapshot)
	}
	if err != nil {
		return err
	}
	if object.OrganizationID != snapshot.Attempt.OrganizationID || object.Reference.Key != snapshot.Attempt.PayloadObjectKey || object.Reference.Validate(string(snapshot.Attempt.OrganizationID)) != nil {
		return artifact.ErrIntegrity
	}
	snapshot.Payload = &object.Reference
	return nil
}

func ReadTenantAttemptAttachmentPayload(ctx context.Context, r tenantstore.Reader, snapshot *TenantAttemptSnapshot) error {
	attempt := snapshot.Attempt
	id := NewRunDetailReference(attachment.ArchiveReference{Key: attempt.PayloadObjectKey}, string(attempt.OrganizationID), "").AttachmentID
	row, err := r.Attachment(ctx, snapshot.Event.ProjectID, id)
	if errors.Is(err, corestore.ErrNotFound) {
		snapshot.RecoverPayload = true
		return nil
	}
	if err != nil {
		return err
	}
	ref := attachment.ArchiveReference{Key: row.ObjectKey, SHA256: row.Metadata.SHA256, CompressedSize: row.Metadata.Size,
		UncompressedSize: row.UncompressedSize, Codec: row.Codec, SchemaVersion: int(row.SchemaVersion),
		Kind: row.Kind, Retention: row.Retention, BindingID: row.StorageBindingID, CustomerOwned: row.CustomerOwned}
	if row.OrganizationID != attempt.OrganizationID || row.Metadata.ProjectID != string(snapshot.Event.ProjectID) || row.Metadata.ID != id ||
		row.Metadata.DeletedAt != nil || row.ObjectDeletedAt != nil || ref.Key != attempt.PayloadObjectKey || ref.Validate(string(attempt.OrganizationID)) != nil ||
		ref.SchemaVersion != 1 || ref.BindingID != "" || ref.CustomerOwned {
		return artifact.ErrIntegrity
	}
	snapshot.Payload = &ref
	return nil
}

type NativeRunEventReader interface {
	RunEvent(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (eventstore.Event, error)
}

func ExplanationRun(data tracker.NativeRunData) tracker.NativeRunData {
	result := CompactRun(data)
	runtime := data.Runtime.WithoutActivitySpans()
	if runtime != nil && result.Runtime != nil {
		result.Runtime.Landing = ExplanationLanding(runtime.Landing)
	}
	result.ArtifactIDs = result.ArtifactIDs[:min(len(result.ArtifactIDs), 16)]
	return result
}

func ExplanationLanding(value *tracker.NativeLandingReceipt) *tracker.NativeLandingReceipt {
	result := BoundedChangeLanding(value)
	if result == nil {
		return nil
	}
	result.Pipeline, result.Packages, result.SecurityAudit = nil, nil, nil
	if result.Gate != nil {
		result.Gate.Evidence, result.Gate.Pipeline = nil, nil
	}
	if result.CI != nil {
		ci := *result.CI
		for _, values := range []*[]string{&ci.RequiredChecks, &ci.PendingChecks, &ci.MissingChecks, &ci.FailedChecks} {
			*values = slices.Clone((*values)[:min(len(*values), 16)])
			for i := range *values {
				(*values)[i] = hubapi.BoundedFailureText((*values)[i], 256)
			}
		}
		result.CI = &ci
	}
	return result
}

func BoundedChangeCommand(command *gate.CommandResult) *gate.CommandResult {
	if command == nil {
		return nil
	}
	result := tracker.PublicGateResult(*command)
	return &result
}

func BoundedChangeLanding(landing *tracker.NativeLandingReceipt) *tracker.NativeLandingReceipt {
	if landing == nil {
		return nil
	}
	result := *landing
	result.Gate = BoundedChangeCommand(result.Gate)
	return &result
}

type ReleaseDeliveryContractFacts struct {
	Delivery        *eventstore.ReleaseDelivery `json:"-"`
	DependsOnSource bool                        `json:"-"`
	SourceTerminal  bool                        `json:"-"`
	Change          eventstore.Change           `json:"-"`
}
