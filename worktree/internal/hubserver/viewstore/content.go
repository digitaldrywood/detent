package viewstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Content struct{ store attachment.DetailStore }

func NewContent(store attachment.DetailStore) *Content { return &Content{store: store} }

func (c *Content) PrepareBoard(ctx context.Context, card tracker.NativeBoardCard) (attachment.ArchiveReference, error) {
	if c.store == nil {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	if card.Issue.OrganizationID == "" || card.Issue.ProjectID == "" || card.Issue.WorkItemID == "" || card.Issue.Revision < 1 {
		return attachment.ArchiveReference{}, ErrInvalidView
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return attachment.ArchiveReference{}, fmt.Errorf("encode board payload: %w", ErrInvalidView)
	}
	ref, err := c.store.StoreDetail(ctx, string(card.Issue.OrganizationID), raw)
	if err != nil {
		return ref, fmt.Errorf("store board payload: %w", err)
	}
	if ref.Validate(string(card.Issue.OrganizationID)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(raw)) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	verified, err := c.store.ReadDetail(ctx, string(card.Issue.OrganizationID), ref)
	if err != nil {
		return attachment.ArchiveReference{}, fmt.Errorf("verify board payload: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if !bytes.Equal(raw, verified) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	return ref, nil
}

func (c *Content) Board(ctx context.Context, organization tracker.OrganizationID, ref attachment.ArchiveReference) (tracker.NativeBoardCard, error) {
	var card tracker.NativeBoardCard
	if c.store == nil {
		return card, artifact.ErrStorage
	}
	if ref.Validate(string(organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 {
		return card, artifact.ErrIntegrity
	}
	raw, err := c.store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return card, fmt.Errorf("read board payload: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	if int64(len(raw)) != ref.UncompressedSize || json.Unmarshal(raw, &card) != nil || card.Issue.OrganizationID != organization || card.Issue.ProjectID == "" || card.Issue.WorkItemID == "" || card.Issue.Revision < 1 {
		return tracker.NativeBoardCard{}, artifact.ErrIntegrity
	}
	return card, nil
}

func (c *Content) BoardRecord(ctx context.Context, row BoardRecord) (tracker.NativeBoardCard, error) {
	card, err := c.Board(ctx, row.OrganizationID, row.Payload)
	if err != nil {
		return card, err
	}
	if card.Issue.ProjectID != row.ProjectID || card.Issue.WorkItemID != row.WorkItemID || int64(card.Issue.Revision) != row.SourceRevision || card.Issue.State != row.Position.State || card.Issue.Archived != row.Archived || card.Issue.Terminal != row.Terminal {
		return tracker.NativeBoardCard{}, artifact.ErrIntegrity
	}
	verification := ""
	if card.Issue.Verification != nil {
		verification = card.Issue.Verification.State
	}
	if card.Issue.Actor.PrincipalID != row.ActorPrincipalID || card.Issue.Type != row.IssueType || verification != row.VerificationState || (card.Issue.Priority == nil) != (row.Priority == nil) || card.Issue.Priority != nil && *card.Issue.Priority != *row.Priority || !sameBoardValues(card.Issue.Labels, row.Labels) || !sameBoardValues(card.Issue.Assignees, row.Assignees) {
		return tracker.NativeBoardCard{}, artifact.ErrIntegrity
	}
	return card, nil
}

func sameBoardValues(left, right []string) bool {
	left = slices.Clone(left)
	right = slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(slices.Compact(left), slices.Compact(right))
}
