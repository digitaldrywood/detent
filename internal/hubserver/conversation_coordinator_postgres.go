package hubserver

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *conversationTurnCoordinator) coordinatorMessages(ctx context.Context, filter conversationstore.MessageFilter) ([]conversationMessageRecord, error) {
	organization, err := c.service.lifecycleOrganization()
	if err != nil {
		return nil, err
	}
	var messages []conversationMessageRecord
	refs := map[string]attachment.ArchiveReference{}
	err = c.service.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.ConversationByID(ctx, filter.ConversationID)
		if err != nil {
			return err
		}
		page, err := r.FilteredMessages(ctx, filter)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			ids = append(ids, item.ID)
		}
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return conversationContentUnavailable()
		}
		attachments, err := owner.MessageAttachments(ctx, current.ProjectID, current.ID, ids)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			message := conversationMessageFromStore(item)
			if item.ContentReference != "" {
				object, err := r.ConversationContent(ctx, item.ContentReference)
				if err != nil {
					return err
				}
				refs[item.ContentReference] = object.Reference
			}
			for _, a := range attachments[item.ID] {
				message.Attachments = append(message.Attachments, conversationAttachmentFromStore(a))
			}
			references, err := r.MessageReferences(ctx, current.ID, item.ID)
			if err != nil {
				return err
			}
			for _, reference := range references {
				path := "/chat/c/" + url.PathEscape(reference.TargetID)
				if reference.Kind == conversation.ReferenceIssue {
					item, err := r.WorkItem(ctx, current.ProjectID, tracker.NativeWorkItemID(reference.TargetID))
					if err != nil {
						return err
					}
					org, err := r.AccessOrganization(ctx)
					if err != nil {
						return err
					}
					project, err := r.Project(ctx, current.ProjectID)
					if err != nil {
						return err
					}
					path = issueBrowserPath(org.Slug, project.Slug, int(item.Number))
				}
				message.References = append(message.References, conversationMessageReference{Kind: reference.Kind, ID: reference.TargetID, Label: reference.Label, URL: path})
			}
			messages = append(messages, message)
		}
		return nil
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	for i := range messages {
		if ref, ok := refs[messages[i].ContentID]; ok {
			var content conversationContent
			if err := c.service.server.readConversationObject(ctx, string(organization), ref, &content); err != nil {
				return nil, err
			}
			messages[i].Text, messages[i].Data = content.Text, content.Data
		}
	}
	slices.SortFunc(messages, func(a, b conversationMessageRecord) int {
		if a.Seq < b.Seq {
			return -1
		}
		if a.Seq > b.Seq {
			return 1
		}
		return 0
	})
	return messages, nil
}

func (c *conversationTurnCoordinator) attachmentContent(ctx context.Context, id string) ([]byte, error) {
	organization, err := c.service.lifecycleOrganization()
	if err != nil {
		return nil, err
	}
	var object conversationstore.ContentObject
	err = c.service.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		object, err = r.ConversationContent(ctx, id)
		return err
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	if c.service.store.contents == nil {
		return nil, conversationContentUnavailable()
	}
	return c.service.store.contents.ReadContent(ctx, string(organization), object.Reference)
}

func (c *conversationTurnCoordinator) modelChoiceGranted(ctx context.Context, r tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
	d := c.service.server.database
	if d.plans() == nil || d.hostedOrganization != organization {
		return false, nil
	}
	entitlement, err := d.hostedEntitlementRecord(ctx, r, now)
	return slices.Contains(entitlement.Features, "model_choice"), err
}

func (c *conversationTurnCoordinator) modelChoices(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, now time.Time) ([]conversationModel, error) {
	owner, ok := r.(runnerstore.FleetReader)
	if !ok {
		return nil, conversationContentUnavailable()
	}
	runners, err := owner.FleetRecords(ctx, 1000)
	if err != nil {
		return nil, err
	}
	var models []conversationModel
	seen := map[string]bool{}
	for _, candidate := range runners {
		runner := candidate.Identity
		if candidate.CredentialRevoked || (candidate.CredentialExpiresAt != nil && !now.Before(*candidate.CredentialExpiresAt)) {
			continue
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, runner.RunnerID, project, now)
		if err != nil {
			return nil, err
		}
		if !allowed || runner.State != "active" {
			continue
		}
		for _, report := range candidate.Reports {
			if !reportFresh(report, now) {
				continue
			}
			for _, model := range report.Models {
				model = strings.TrimSpace(model)
				if model == "" || seen[model] {
					continue
				}
				seen[model] = true
				models = append(models, conversationModelFromReport(report, model))
			}
		}
	}
	s := c.service.server
	if s.hasLunaCoordinator() {
		models = slices.DeleteFunc(models, func(model conversationModel) bool { return model.ID == genkitbackend.Model })
		models = append(models, conversationModel{ID: genkitbackend.Model, Label: "Luna", Provider: "openai", Efforts: []string{"low", "medium"}, DefaultEffort: "low", BackendDefault: true})
	}
	if model := s.conversationDefaultModel(); model != "" && !slices.ContainsFunc(models, func(choice conversationModel) bool { return choice.ID == model }) {
		models = append(models, conversationModel{ID: model, Label: model})
	}
	return models, nil
}

func coordinatorMonthlyBudget(ctx context.Context, reader billingstore.Reader, project tracker.ProjectID, now time.Time) (budget.MonthlyDecision, error) {
	var input billingstore.MonthlyBudgetInput
	var err error
	input.Exposure = budget.CostExposure{LunaAPI: true}
	input.OrganizationPolicy, err = reader.BillingMonthlyPolicy(ctx, "")
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return budget.MonthlyDecision{}, err
	}
	input.ProjectPolicy, err = reader.BillingMonthlyPolicy(ctx, project)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return budget.MonthlyDecision{}, err
	}
	if !input.OrganizationPolicy.Policy.Enabled && !input.ProjectPolicy.Policy.Enabled {
		return budget.MonthlyDecision{Allowed: true}, nil
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	input.OrganizationSpend, err = reader.BillingMonthlySpend(ctx, "", month)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	input.ProjectSpend, err = reader.BillingMonthlySpend(ctx, project, month)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	return billingstore.EvaluateMonthlyBudget(input)
}
