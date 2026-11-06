package hubserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *conversationService) readPublicationConversation(ctx context.Context, id string) (conversationRecord, error) {
	organization, err := c.lifecycleOrganization()
	if err != nil {
		return conversationRecord{}, err
	}
	return c.readPublicationConversationForOrganization(ctx, organization, id)
}

func (c *conversationService) readPublicationConversationForOrganization(ctx context.Context, organization tracker.OrganizationID, id string) (conversationRecord, error) {
	var record conversationRecord
	var ref attachment.ArchiveReference
	err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.ConversationByID(ctx, id)
		if err != nil {
			return err
		}
		record = conversationRecordFromStore(stored)
		if record.ContentID != "" && (record.Execution.Status == "" || record.Preferences == (conversation.Preferences{})) {
			content, err := r.ConversationContent(ctx, record.ContentID)
			if err != nil {
				return err
			}
			ref = content.Reference
		}
		if record.WorkItemID != "" {
			item, err := r.WorkItem(ctx, record.ProjectID, tracker.NativeWorkItemID(record.WorkItemID))
			if err != nil {
				return err
			}
			project, err := r.Project(ctx, record.ProjectID)
			if err != nil {
				return err
			}
			record.WorkItem = &conversationWorkItem{ID: string(item.NativeID), Identifier: fmt.Sprintf("%s#%d", project.Name, item.Number), Title: item.Title}
			if item.WorkflowStateName != nil {
				record.WorkItem.Lane = *item.WorkflowStateName
			}
		}
		return nil
	})
	if err != nil {
		return record, translateConversationError(err)
	}
	if record.ContentID != "" && (record.Execution.Status == "" || record.Preferences == (conversation.Preferences{})) {
		var content conversationContent
		if err := c.server.readConversationObject(ctx, string(organization), ref, &content); err != nil {
			return record, err
		}
		var payload conversationRecordContent
		if err := json.Unmarshal(content.Data, &payload); err != nil {
			return record, err
		}
		if record.Execution.Status == "" {
			record.Execution = payload.Execution
		}
		if record.Preferences == (conversation.Preferences{}) {
			record.Preferences = payload.Preferences.Normalized()
		}
	}
	return record, nil
}

func (c *conversationService) readPublicationMessage(ctx context.Context, organization tracker.OrganizationID, id, messageID string) (conversationMessageRecord, error) {
	var message conversationMessageRecord
	var ref attachment.ArchiveReference
	err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.Message(ctx, id, messageID)
		if err != nil {
			return err
		}
		message = conversationMessageFromStore(stored)
		if message.ContentID != "" {
			content, err := r.ConversationContent(ctx, message.ContentID)
			if err != nil {
				return err
			}
			ref = content.Reference
		}
		return nil
	})
	if err != nil {
		return message, translateConversationError(err)
	}
	if message.ContentID != "" {
		var content conversationContent
		if err := c.server.readConversationObject(ctx, string(organization), ref, &content); err != nil {
			return message, err
		}
		message.Text, message.Data = content.Text, content.Data
	}
	return message, nil
}
