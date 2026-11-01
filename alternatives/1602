package eventstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrInvalidRecord = errors.New("invalid event or change record")

type Content struct{ store attachment.DetailStore }

func NewContent(store attachment.DetailStore) *Content { return &Content{store: store} }

func (c *Content) prepare(ctx context.Context, organization tracker.OrganizationID, value any) (attachment.ArchiveReference, error) {
	if c.store == nil {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return attachment.ArchiveReference{}, fmt.Errorf("encode history payload: %w", ErrInvalidRecord)
	}
	ref, err := c.store.StoreDetail(ctx, string(organization), raw)
	if err != nil {
		return ref, fmt.Errorf("store history payload: %w", err)
	}
	if ref.Validate(string(organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(raw)) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	verified, err := c.store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return attachment.ArchiveReference{}, fmt.Errorf("verify history payload: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if !bytes.Equal(verified, raw) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	return ref, nil
}

func (c *Content) read(ctx context.Context, organization tracker.OrganizationID, ref attachment.ArchiveReference, value any) error {
	if c.store == nil {
		return artifact.ErrStorage
	}
	if ref.Validate(string(organization)) != nil {
		return artifact.ErrIntegrity
	}
	raw, err := c.store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return fmt.Errorf("read history payload: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if int64(len(raw)) != ref.UncompressedSize {
		return artifact.ErrIntegrity
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("decode history payload: %w", artifact.ErrIntegrity)
	}
	return nil
}

func Reference(key, digest string, compressed, content int64) attachment.ArchiveReference {
	return attachment.ArchiveReference{Key: key, SHA256: digest, CompressedSize: compressed, UncompressedSize: content, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
}

func (c *Content) PrepareEvent(ctx context.Context, organization tracker.OrganizationID, raw []byte) (Event, error) {
	var value tracker.CollaborationEvent
	if json.Unmarshal(raw, &value) != nil || value.OrganizationID != organization || value.ID == "" || value.ProjectID == "" || value.AggregateID == "" || value.SchemaVersion != 1 || value.AggregateSequence < 0 {
		return Event{}, ErrInvalidRecord
	}
	ref, err := c.prepare(ctx, organization, value)
	row := Event{OrganizationID: organization, ProjectID: value.ProjectID, WorkItemID: value.AggregateID, ID: value.ID, Sequence: value.AggregateSequence, Type: value.Type, SchemaVersion: int64(value.SchemaVersion), ActorKind: value.Actor.Kind, ActorPrincipalID: value.Actor.PrincipalID, RecordedAt: value.RecordedAt, EventRevision: int64(value.Data.Revision), FromState: value.Data.FromState, ToState: value.Data.ToState}
	if value.Data.Change != nil {
		row.ChangeID = value.Data.Change.ChangeID
		row.VersionID = value.Data.Change.VersionID
		row.HeadSHA = value.Data.Change.HeadSHA
	}
	if value.Data.Run != nil {
		row.RunID = value.Data.Run.RunID
		row.AttemptID = value.Data.Run.AttemptID
	}
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) Event(ctx context.Context, row Event) (tracker.CollaborationEvent, error) {
	var value tracker.CollaborationEvent
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil && (value.ID != row.ID || value.OrganizationID != row.OrganizationID || value.ProjectID != row.ProjectID || value.AggregateID != row.WorkItemID || value.Type != row.Type || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID) {
		return value, artifact.ErrIntegrity
	}
	value.AggregateSequence, value.RecordedAt = row.Sequence, row.RecordedAt
	return value, err
}

func (c *Content) PrepareChange(ctx context.Context, organization tracker.OrganizationID, raw []byte) (Change, error) {
	var value tracker.ChangeRequest
	if json.Unmarshal(raw, &value) != nil || value.OrganizationID != organization || value.ID == "" || value.ProjectID == "" || value.WorkItemID == "" {
		return Change{}, ErrInvalidRecord
	}
	ref, err := c.prepare(ctx, organization, value)
	row := Change{OrganizationID: organization, ProjectID: value.ProjectID, WorkItemID: value.WorkItemID, ID: value.ID, Revision: int64(value.Revision), Title: value.Title, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt}
	if value.CurrentVersion != "" {
		row.CurrentVersionID = new(value.CurrentVersion)
	}
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) Change(ctx context.Context, row Change) (tracker.ChangeRequest, error) {
	var value tracker.ChangeRequest
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil && (value.ID != row.ID || value.OrganizationID != row.OrganizationID || value.ProjectID != row.ProjectID || value.WorkItemID != row.WorkItemID || value.Title != row.Title) {
		return value, artifact.ErrIntegrity
	}
	value.Revision, value.CreatedAt, value.UpdatedAt = tracker.Revision(row.Revision), row.CreatedAt, row.UpdatedAt
	value.CurrentVersion = ""
	if row.CurrentVersionID != nil {
		value.CurrentVersion = *row.CurrentVersionID
	}
	return value, err
}

func (c *Content) PrepareVersion(ctx context.Context, organization tracker.OrganizationID, raw []byte) (Version, error) {
	var value tracker.ChangeVersion
	if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.ChangeID == "" || value.Number < 1 || value.HeadSHA == "" || value.PolicyID == "" {
		return Version{}, ErrInvalidRecord
	}
	ref, err := c.prepare(ctx, organization, value)
	row := Version{OrganizationID: organization, ID: value.ID, ChangeID: value.ChangeID, Number: value.Number, HeadSHA: value.HeadSHA, BaseSHA: value.BaseSHA, MergeBaseSHA: value.MergeBaseSHA, Repository: value.Repository, PolicyRevisionID: value.PolicyID, AttemptID: value.AttemptID, ActorKind: value.Actor.Kind, ActorPrincipalID: value.Actor.PrincipalID, CreatedAt: value.CreatedAt}
	if value.External != nil {
		row.ExternalProvider = value.External.Provider
		row.ExternalID = value.External.ID
		row.ExternalURL = value.External.URL
	}
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) Version(ctx context.Context, row Version) (tracker.ChangeVersion, error) {
	var value tracker.ChangeVersion
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil && (value.ID != row.ID || value.ChangeID != row.ChangeID || value.Number != row.Number || value.HeadSHA != row.HeadSHA || value.PolicyID != row.PolicyRevisionID || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID) {
		return value, artifact.ErrIntegrity
	}
	value.CreatedAt = row.CreatedAt
	return value, err
}

func (c *Content) PrepareEvidence(ctx context.Context, row Evidence, raw []byte) (Evidence, error) {
	var ref attachment.ArchiveReference
	var err error
	switch row.Kind {
	case "review":
		var value tracker.ChangeReview
		if json.Unmarshal(raw, &value) != nil || value.ID == "" {
			return Evidence{}, ErrInvalidRecord
		}
		row.ActorKind, row.ActorPrincipalID, row.Decision, row.CreatedAt = value.Actor.Kind, value.Actor.PrincipalID, value.Decision, value.CreatedAt
		if value.VersionID != "" {
			row.VersionID = new(value.VersionID)
		}
		ref, err = c.prepare(ctx, row.OrganizationID, value)
	case "check":
		var value tracker.ChangeCheck
		if json.Unmarshal(raw, &value) != nil || value.CheckRunID == "" {
			return Evidence{}, ErrInvalidRecord
		}
		row.ActorKind, row.ActorPrincipalID, row.CreatedAt = value.Actor.Kind, value.Actor.PrincipalID, value.ReceivedAt
		row.VersionID = new(value.VersionID)
		row.SourceKey = new(value.CheckRunID)
		ref, err = c.prepare(ctx, row.OrganizationID, value)
	case "discussion":
		var value tracker.ChangeDiscussion
		if json.Unmarshal(raw, &value) != nil || value.ID == "" {
			return Evidence{}, ErrInvalidRecord
		}
		row.ActorKind, row.ActorPrincipalID, row.CreatedAt = value.Actor.Kind, value.Actor.PrincipalID, value.CreatedAt
		if value.VersionID != "" {
			row.VersionID = new(value.VersionID)
		}
		if value.Provenance != nil {
			row.SourceKey = new(value.Provenance.Provider + ":" + value.Provenance.ExternalID)
		}
		ref, err = c.prepare(ctx, row.OrganizationID, value)
	default:
		return Evidence{}, ErrInvalidRecord
	}
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) Evidence(ctx context.Context, row Evidence) (json.RawMessage, error) {
	var value json.RawMessage
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	return value, err
}

func (c *Content) PrepareCollaborationVersion(ctx context.Context, row CollaborationVersion, raw []byte) (CollaborationVersion, error) {
	var value json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return CollaborationVersion{}, ErrInvalidRecord
	}
	ref, err := c.prepare(ctx, row.OrganizationID, value)
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) CollaborationVersion(ctx context.Context, row CollaborationVersion) (json.RawMessage, error) {
	var value json.RawMessage
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	return value, err
}

func (c *Content) PrepareReceipt(ctx context.Context, row Receipt, raw []byte) (Receipt, error) {
	var value json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return Receipt{}, ErrInvalidRecord
	}
	ref, err := c.prepare(ctx, row.OrganizationID, value)
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) Receipt(ctx context.Context, row Receipt) (json.RawMessage, error) {
	var value json.RawMessage
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	return value, err
}
