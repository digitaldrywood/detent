package conversations

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ResolveMessageReferences(ctx context.Context, r tenantstore.Reader, record ConversationRecord, text string) ([]ConversationMessageReference, error) {
	references := conversation.ExtractReferences(text)
	resolved := make([]ConversationMessageReference, 0, len(references))
	seen := map[string]bool{}
	for _, reference := range references {
		var target string
		switch reference.Kind {
		case conversation.ReferenceConversation:
			if reference.ConversationID == record.ID {
				continue
			}
			if _, err := r.ConversationByID(ctx, reference.ConversationID); errors.Is(err, corestore.ErrNotFound) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("resolve conversation reference: %w", err)
			}
			target = reference.ConversationID
		case conversation.ReferenceIssue:
			project := record.ProjectID
			if reference.Project != "" {
				if reference.Organization != "" && !strings.EqualFold(reference.Organization, string(record.OrganizationID)) {
					continue
				}
				named, err := r.ProjectIDByName(ctx, reference.Project)
				if errors.Is(err, corestore.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("resolve reference project: %w", err)
				}
				project = named
			}
			item, err := r.WorkItemByNumber(ctx, project, reference.Number)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("resolve issue reference: %w", err)
			}
			target = string(item.NativeID)
		default:
			continue
		}
		key := string(reference.Kind) + "\x00" + target
		if seen[key] {
			continue
		}
		seen[key] = true
		path, err := MessageReferenceURL(ctx, r, reference.Kind, target)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, ConversationMessageReference{Kind: reference.Kind, ID: target, Label: reference.Text, URL: path})
	}
	return resolved, nil
}

func MessageReferenceURL(ctx context.Context, r tenantstore.Reader, kind conversation.ReferenceKind, target string) (string, error) {
	if kind != conversation.ReferenceIssue {
		return "/chat/c/" + url.PathEscape(target), nil
	}
	item, err := r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(target))
	if err != nil {
		return "", fmt.Errorf("read referenced issue: %w", err)
	}
	organization, err := r.AccessOrganization(ctx)
	if err != nil {
		return "", fmt.Errorf("read reference organization: %w", err)
	}
	project, err := r.Project(ctx, item.ProjectID)
	if err != nil {
		return "", fmt.Errorf("read reference project: %w", err)
	}
	return fmt.Sprintf("/%s/%s/issues/%d", url.PathEscape(organization.Slug), url.PathEscape(project.Slug), item.Number), nil
}

func ReplaceMessageReferences(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, message ConversationMessageRecord, now time.Time) error {
	refs := make([]conversationstore.MessageReference, 0, len(message.References))
	for index, ref := range message.References {
		refs = append(refs, conversationstore.MessageReference{OrganizationID: organization, ConversationID: message.ConversationID, MessageID: message.ID, Position: int64(index), Kind: ref.Kind, TargetID: ref.ID, Label: ref.Label, CreatedAt: now})
	}
	return w.ReplaceMessageReferences(ctx, message.ConversationID, message.ID, refs, now)
}
