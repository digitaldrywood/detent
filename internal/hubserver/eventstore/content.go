package eventstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/policy"
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

func (c *Content) PrepareLinkedIssueSnapshot(ctx context.Context, organization tracker.OrganizationID, value tracker.GitHubIssueSnapshot) (attachment.ArchiveReference, error) {
	return c.prepare(ctx, organization, value)
}

func (c *Content) LinkedIssueSnapshot(ctx context.Context, organization tracker.OrganizationID, ref attachment.ArchiveReference) (tracker.GitHubIssueSnapshot, error) {
	var value tracker.GitHubIssueSnapshot
	err := c.read(ctx, organization, ref, &value)
	return value, err
}

func (c *Content) PrepareProjectPolicy(ctx context.Context, organization tracker.OrganizationID, value policy.Descriptor) (policystore.Revision, error) {
	ref, err := c.prepare(ctx, organization, value)
	return policystore.Revision{OrganizationID: organization, PolicyID: value.ID, GitHubPullRequest: new(value.Gates.GitHubPullRequest), PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize}, err
}

func (c *Content) ProjectPolicy(ctx context.Context, row policystore.Revision) (policy.Descriptor, error) {
	var value policy.Descriptor
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil && value.ID != row.PolicyID {
		return policy.Descriptor{}, artifact.ErrIntegrity
	}
	return value, err
}

func (c *Content) PrepareReviewPolicy(ctx context.Context, row ReviewPolicy, value tracker.ChangeReviewPolicy) (ReviewPolicy, error) {
	if value.ID == "" || value.PolicyID != row.PolicyRevisionID {
		return ReviewPolicy{}, ErrInvalidRecord
	}
	ref, err := c.prepare(ctx, row.OrganizationID, value)
	row.ReviewPolicyID = value.ID
	row.PayloadObjectKey, row.PayloadSHA256 = ref.Key, ref.SHA256
	row.PayloadBytes, row.PayloadContentBytes = ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) ReviewPolicy(ctx context.Context, row ReviewPolicy) (tracker.ChangeReviewPolicy, error) {
	var value tracker.ChangeReviewPolicy
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil && (value.ID != row.ReviewPolicyID || value.PolicyID != row.PolicyRevisionID) {
		return tracker.ChangeReviewPolicy{}, artifact.ErrIntegrity
	}
	return value, err
}

func (c *Content) PrepareLandingReceipt(ctx context.Context, row LandingReceipt, value tracker.NativeLandingReceipt) (LandingReceipt, error) {
	if row.OrganizationID == "" || row.AttemptID == "" || row.VersionID == "" || row.Repository == "" || row.BaseRef == "" || value.VersionID != row.VersionID || value.MergeSHA == "" || value.BaseRef != row.BaseRef {
		return LandingReceipt{}, ErrInvalidRecord
	}
	ref, err := c.prepare(ctx, row.OrganizationID, value)
	row.MergeSHA = value.MergeSHA
	row.PayloadObjectKey, row.PayloadSHA256 = ref.Key, ref.SHA256
	row.PayloadBytes, row.PayloadContentBytes = ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) LandingReceipt(ctx context.Context, row LandingReceipt) (tracker.NativeLandingReceipt, error) {
	var value tracker.NativeLandingReceipt
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil && (value.VersionID != row.VersionID || value.MergeSHA != row.MergeSHA || row.BaseRef != "" && value.BaseRef != row.BaseRef) {
		return tracker.NativeLandingReceipt{}, artifact.ErrIntegrity
	}
	return value, err
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
	row = ProjectChangePush(row, value)
	if value.Data.Run != nil {
		row.RunID = value.Data.Run.RunID
		row.AttemptID = value.Data.Run.AttemptID
		if value.Data.Run.Runtime != nil && value.Data.Run.Runtime.Landing != nil {
			landing := value.Data.Run.Runtime.Landing
			row.LandingWaiting = landing.Waiting
			row.LandingChangeID = landing.ChangeID
			row.LandingVersionID = landing.VersionID
			row.LandingHeadSHA = landing.HeadSHA
		}
	}
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	row.Report = reportstore.ProjectEvent(value)
	return row, err
}

func (c *Content) Event(ctx context.Context, row Event) (tracker.CollaborationEvent, error) {
	var value tracker.CollaborationEvent
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil {
		if value.ID != row.ID || value.OrganizationID != row.OrganizationID || value.ProjectID != row.ProjectID || value.AggregateID != row.WorkItemID || value.Type != row.Type || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID {
			return value, artifact.ErrIntegrity
		}
		if row.PushKnown {
			var checkpoint tracker.NativeCheckpoint
			if value.Data.Run != nil && value.Data.Run.Handoff != nil {
				checkpoint = *value.Data.Run.Handoff
			}
			var reference tracker.NativeChangeReference
			if checkpoint.Change != nil {
				reference = *checkpoint.Change
			}
			if row.PushChangeID != reference.ChangeID || row.PushVersionID != reference.VersionID || row.PushHeadSHA != reference.HeadSHA || row.PushExternalEffect != checkpoint.ExternalEffect || row.PushEffectState != checkpoint.EffectState {
				return value, artifact.ErrIntegrity
			}
		}
		var landing tracker.NativeLandingReceipt
		if value.Data.Run != nil && value.Data.Run.Runtime != nil && value.Data.Run.Runtime.Landing != nil {
			landing = *value.Data.Run.Runtime.Landing
		}
		if row.LandingWaiting != landing.Waiting || row.LandingChangeID != landing.ChangeID || row.LandingVersionID != landing.VersionID || row.LandingHeadSHA != landing.HeadSHA {
			return value, artifact.ErrIntegrity
		}
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
	if value.Landed != nil {
		row.LandedVersionID = new(value.Landed.VersionID)
		landedAt := value.Landed.LandedAt
		row.LandedAt = &landedAt
		row.LandedHeadSHA = new(value.Landed.HeadSHA)
		row.LandedMergeSHA = new(value.Landed.MergeSHA)
		row.LandedBaseRef = new(value.Landed.BaseRef)
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

func (c *Content) PrepareSourceBundle(ctx context.Context, organization tracker.OrganizationID, bundle []byte) (attachment.ArchiveReference, error) {
	if len(bundle) == 0 || len(bundle) > tracker.MaxChangeSourceBytes || c.store == nil {
		return attachment.ArchiveReference{}, ErrInvalidRecord
	}
	ref, err := c.store.StoreDetail(ctx, string(organization), bundle)
	if err != nil {
		return attachment.ArchiveReference{}, fmt.Errorf("store change source: %w", err)
	}
	if ref.Validate(string(organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(bundle)) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	verified, err := c.store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return attachment.ArchiveReference{}, fmt.Errorf("verify change source: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if !bytes.Equal(verified, bundle) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	return ref, nil
}

func (c *Content) SourceBundle(ctx context.Context, organization tracker.OrganizationID, row Version, source tracker.ChangeSource) ([]byte, error) {
	if c.store == nil || row.SourceObjectKey == "" || row.SourceSHA256 == "" || row.SourceBytes <= 0 || row.SourceContentBytes <= 0 {
		return nil, artifact.ErrIntegrity
	}
	ref := Reference(row.SourceObjectKey, row.SourceSHA256, row.SourceBytes, row.SourceContentBytes)
	if ref.Validate(string(organization)) != nil {
		return nil, artifact.ErrIntegrity
	}
	bundle, err := c.store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return nil, fmt.Errorf("read change source: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if int64(len(bundle)) != ref.UncompressedSize || source.Validate(row.BaseSHA, row.HeadSHA, bundle) != nil {
		return nil, artifact.ErrIntegrity
	}
	return bundle, nil
}

func (c *Content) Version(ctx context.Context, row Version) (tracker.ChangeVersion, error) {
	var value tracker.ChangeVersion
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	if err == nil && (value.ID != row.ID || value.ChangeID != row.ChangeID || value.Number != row.Number || value.HeadSHA != row.HeadSHA || value.PolicyID != row.PolicyRevisionID || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID) {
		return value, artifact.ErrIntegrity
	}
	if err == nil && (value.Source == nil && (row.SourceObjectKey != "" || row.SourceSHA256 != "" || row.SourceBytes != 0 || row.SourceContentBytes != 0) || value.Source != nil && (row.SourceObjectKey == "" || row.SourceSHA256 == "" || row.SourceBytes <= 0 || value.Source.Bytes != row.SourceContentBytes)) {
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
		row, err = ProjectChangeReview(row, value)
		if err != nil {
			return Evidence{}, err
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
		if escape := value.Escape; escape != nil {
			row.ReportEscape = &reportstore.EscapeProjection{OccurrenceID: escape.OccurrenceID, Kind: escape.Kind, Cause: escape.Cause, ObservedAt: escape.ObservedAt.UTC().Truncate(time.Microsecond), InstanceID: escape.InstanceID, HumanOverride: escape.HumanOverride}
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
	if err == nil && row.Kind == "review" {
		var review tracker.ChangeReview
		if json.Unmarshal(value, &review) != nil {
			return nil, artifact.ErrIntegrity
		}
		projected, err := ProjectChangeReview(row, review)
		if err != nil {
			return nil, err
		}
		if row.ReviewKind != "" && (row.ReviewKind != projected.ReviewKind || row.ReviewLanding != projected.ReviewLanding) || row.ReviewSessionKnown && (row.ReviewSessionID != projected.ReviewSessionID || row.ReviewSessionMachine != projected.ReviewSessionMachine) {
			return nil, artifact.ErrIntegrity
		}
	}
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
	var err error
	row.ResponsePending, row.ResponseRequestID, err = receiptstore.ResponseFacts(raw)
	if err != nil {
		return Receipt{}, err
	}
	ref, err := c.prepare(ctx, row.OrganizationID, value)
	row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	return row, err
}

func (c *Content) Receipt(ctx context.Context, row Receipt) (json.RawMessage, error) {
	if row.ResponseExpired {
		return nil, receiptstore.ErrExpired
	}
	if row.ResponseClaimed {
		return nil, mutation.ErrUncertain
	}
	var value json.RawMessage
	err := c.read(ctx, row.OrganizationID, Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), &value)
	return value, err
}

func DecodeChangeReview(row Evidence, raw []byte) (tracker.ChangeReview, error) {
	var value tracker.ChangeReview
	version := ""
	if row.VersionID != nil {
		version = *row.VersionID
	}
	if row.Kind != "review" || json.Unmarshal(raw, &value) != nil || value.ID == "" || value.VersionID != version || value.Decision != row.Decision || value.Actor.Kind != row.ActorKind || value.Actor.PrincipalID != row.ActorPrincipalID || !value.CreatedAt.UTC().Truncate(time.Microsecond).Equal(row.CreatedAt.UTC().Truncate(time.Microsecond)) {
		return value, ErrInvalidRecord
	}
	_, err := ProjectChangeReview(row, value)
	return value, err
}

func ProjectChangeReview(row Evidence, value tracker.ChangeReview) (Evidence, error) {
	results := 0
	for _, present := range []bool{value.SecurityAudit != nil, value.ImplementationReview != nil, value.Validator != nil} {
		if present {
			results++
		}
	}
	if results > 1 {
		return Evidence{}, ErrInvalidRecord
	}
	row.ReviewKind, row.ReviewLanding = "human", false
	row.ReviewSessionKnown, row.ReviewSessionID, row.ReviewSessionMachine = true, 0, value.SessionMachineID
	switch {
	case value.SecurityAudit != nil:
		row.ReviewKind, row.ReviewLanding = "security_audit", value.SecurityAudit.Landing
		row.ReviewSessionID = value.SecurityAudit.SessionID
	case value.ImplementationReview != nil:
		row.ReviewKind = "implementation"
		row.ReviewSessionID = value.ImplementationReview.SessionID
	case value.Validator != nil:
		row.ReviewKind = "validator"
		row.ReviewSessionID = value.Validator.SessionID
	}
	if row.ReviewSessionID < 0 {
		return Evidence{}, ErrInvalidRecord
	}
	return row, nil
}

func ProjectChangePush(row Event, value tracker.CollaborationEvent) Event {
	row.PushKnown = true
	row.PushChangeID, row.PushVersionID, row.PushHeadSHA = "", "", ""
	row.PushExternalEffect, row.PushEffectState = "", ""
	if value.Data.Run != nil && value.Data.Run.Handoff != nil {
		checkpoint := value.Data.Run.Handoff
		row.PushExternalEffect, row.PushEffectState = checkpoint.ExternalEffect, checkpoint.EffectState
		if checkpoint.Change != nil {
			row.PushChangeID, row.PushVersionID, row.PushHeadSHA = checkpoint.Change.ChangeID, checkpoint.Change.VersionID, checkpoint.Change.HeadSHA
		}
	}
	return row
}
