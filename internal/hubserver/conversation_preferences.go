package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentoverride"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Turn preferences, references and the settle sweep.

// conversationReferenceExcerptRunes bounds the message excerpt a
// "referenced from" entry carries, so the listing never republishes a whole
// conversation to a reader who may not open it.
const conversationReferenceExcerptRunes = 200

// conversationModel is one model a conversation may select, as the hub
// assembled it from the enrolled runners' provider reports.
//
// The identifier is what capacity and dispatch match against; everything else
// is what a runner's backend catalog said about it, and
// is absent for a report that carries only identifiers.
type conversationModel struct {
	ID string
	// Label is what a picker shows. It falls back to the identifier, which is
	// the vocabulary operators already use.
	Label string
	// Provider names the vendor, so a picker can group by it.
	Provider string
	// Efforts is the model's own reasoning ladder, least to most.
	Efforts []string
	// DefaultEffort is the effort the reporting runner would use for this
	// model, and is one of Efforts when both are set.
	DefaultEffort string
	// Legacy marks a model the provider has named a successor for.
	Legacy bool
	// BackendDefault marks the model the reporting backend picks when it is
	// given none. It is not the same as the choice's Default flag, which says
	// what "auto" resolves to for this hub: one is the provider's answer and
	// the other is the operator's.
	BackendDefault bool
}

// conversationModelChoices returns the models a conversation may select:
// everything the organization's enrolled runners report for the given
// projects, plus the hub's configured coordinator model when it has one, so a
// hub whose runners have reported nothing still offers the project default.
// The bootstrap and the PATCH validation both read it, so the client is never
// offered a model the hub would then refuse.
//
// The order is the runners' own, deduplicated by first sighting, and the
// configured model goes last when no runner reported it. It used to be sorted
// by identifier, which put "gpt-5.6-sol" above the backend's own default
// "gpt-6-astra" — an alphabet is not a ranking, and a provider lists its
// catalogue in the order it wants read. Sorting is the client's business, and
// what it does with the order is decide where the backend's default sits
// (BackendDefault, `app/lib/preferences.ts`).
func (s *Service) conversationModelChoices(ctx context.Context, reader tenantstore.Reader, projects []tracker.ProjectID, now time.Time) ([]conversationModel, error) {
	models, err := reportedProviderModels(ctx, reader, projects, now)
	if err != nil {
		return nil, err
	}
	if s.hasLunaCoordinator() && (projects == nil || len(projects) > 0) {
		models = slices.DeleteFunc(models, func(model conversationModel) bool { return model.ID == genkitbackend.Model })
		models = append(models, conversationModel{ID: genkitbackend.Model, Label: "Luna", Provider: "openai", Efforts: []string{"low", "medium"}, DefaultEffort: "low", BackendDefault: true})
	}
	if configured := s.conversationDefaultModel(); configured != "" &&
		!slices.ContainsFunc(models, func(model conversationModel) bool { return model.ID == configured }) {
		models = append(models, conversationModel{ID: configured, Label: configured})
	}
	return models, nil
}

func (s *Service) hasLunaCoordinator() bool {
	return s.conversations != nil && s.conversations.config.Backend != nil && s.conversations.config.Model == genkitbackend.Model
}

func (s *Service) conversationModelChoiceGranted(ctx context.Context, reader tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
	if s.database.plans() == nil || s.database.hostedOrganization != organization {
		return false, nil
	}
	entitlement, err := s.database.hostedEntitlementRecord(ctx, reader, now)
	if err != nil {
		return false, err
	}
	return slices.Contains(entitlement.Features, "model_choice"), nil
}

func (s *Service) validateConversationPreferences(ctx context.Context, reader tenantstore.Reader, record conversationstore.Conversation, preferences conversation.Preferences, now time.Time) error {
	models, err := s.conversationModelChoices(ctx, reader, []tracker.ProjectID{record.ProjectID}, now)
	if err != nil {
		return err
	}
	if s.hasLunaCoordinator() {
		if record.WorkItemID != "" {
			models = slices.DeleteFunc(models, func(model conversationModel) bool { return model.ID == genkitbackend.Model })
		} else {
			model := preferences.ModelValue()
			if model == "" {
				model = s.conversationDefaultModel()
			}
			if model == genkitbackend.Model {
				if effort := preferences.EffortValue(); effort != "" && effort != "low" && effort != "medium" {
					return nativePublicInvalid("Conversation preferences are invalid")
				}
			} else {
				granted, err := s.conversationModelChoiceGranted(ctx, reader, record.OrganizationID, now)
				if err != nil {
					return err
				}
				if !granted {
					return nativePublicInvalid("Conversation preferences are invalid")
				}
			}
		}
	}
	if err := conversation.ValidatePreferences(preferences, conversationModelValidationChoices(models)); err != nil {
		return nativePublicInvalid("Conversation preferences are invalid")
	}
	return nil
}

// conversationModelValidationChoices is what ValidatePreferences checks
// against: the identifier plus the effort ladder the model published, so an
// effort the picker offered is not then refused.
func conversationModelValidationChoices(models []conversationModel) []conversation.ModelChoice {
	choices := make([]conversation.ModelChoice, 0, len(models))
	for _, model := range models {
		choices = append(choices, conversation.ModelChoice{ID: model.ID, Efforts: model.Efforts})
	}
	return choices
}

// conversationDefaultModel is the model "auto" resolves to for this hub, or
// an empty string when it does not know one.
func (s *Service) conversationDefaultModel() string {
	if s.conversations == nil {
		return ""
	}
	return strings.TrimSpace(s.conversations.config.Model)
}

// reportedProviderModels lists the models the organization's active runners
// report, with whatever detail their backend catalogs supplied. A nil project
// list means every project in the organization; an empty non-nil list means
// none, so a reader with no grants sees nothing.
//
// The first fresh report to describe a model wins its detail. Two runners that
// report the same model with different ladders are reporting two different
// installations of it, and offering the intersection would hide a level one of
// them can honour while offering the union would promise one the other cannot;
// the hub picks the first and stays honest about where it came from.
func reportedProviderModels(ctx context.Context, reader tenantstore.Reader, projects []tracker.ProjectID, now time.Time) ([]conversationModel, error) {
	models := []conversationModel{}
	if projects != nil && len(projects) == 0 {
		return models, nil
	}
	owner, ok := reader.(runnerstore.ProviderCatalogReader)
	if !ok {
		return nil, conversationContentUnavailable()
	}
	catalog, err := owner.RunnerProviderCatalog(ctx, projects, now)
	if err != nil {
		return nil, fmt.Errorf("list runner provider catalog: %w", err)
	}
	seen := map[string]bool{}
	for _, model := range catalog {
		id := strings.TrimSpace(model.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		report := providercapacity.Report{Provider: model.Provider}
		if model.Detail != nil {
			report.ModelDetails = []providercapacity.ModelDetail{*model.Detail}
		}
		models = append(models, conversationModelFromReport(report, id))
	}
	return models, nil
}

// conversationModelFromReport reads one model out of a report. A report that
// carries no detail for the model yields the identifier alone, which is
// exactly what this path published before detail existed.
func conversationModelFromReport(report providercapacity.Report, model string) conversationModel {
	choice := conversationModel{ID: model, Label: model, Provider: strings.TrimSpace(report.Provider)}
	detail, ok := report.Detail(model)
	if !ok {
		return choice
	}
	if label := strings.TrimSpace(detail.Label); label != "" {
		choice.Label = label
	}
	if provider := strings.TrimSpace(detail.Provider); provider != "" {
		choice.Provider = provider
	}
	choice.Efforts = slices.Clone(detail.ReasoningEfforts)
	choice.DefaultEffort = detail.DefaultReasoningEffort
	choice.Legacy = detail.Legacy
	choice.BackendDefault = detail.Default
	return choice
}

// conversationAgentOverride is the detent-agent override a conversation's
// preferences describe. Auto values are omitted: they mean "use the
// project's configured default", which is exactly an absent field. The model
// is never written: a block-level model overrides every stage, including the
// operator's planning and validation split, so the runner reads the model
// from the bind's preferences for the conversation turn alone.
func conversationAgentOverride(preferences conversation.Preferences) agentoverride.Override {
	return agentoverride.Override{Effort: preferences.EffortValue()}
}

// conversationAgentOverrideBody returns body carrying the conversation's
// preferences as a detent-agent block.
func conversationAgentOverrideBody(body string, preferences conversation.Preferences) string {
	return agentoverride.ApplyToIssueBody(body, conversationAgentOverride(preferences))
}

type conversationPreferenceIssue struct {
	plan    workItemRecordPlan
	fields  workItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
	search  searchstore.Document
	changed bool
}

func (s *Service) prepareConversationPreferenceIssue(ctx context.Context, scope nativeScope, record conversationRecord, preferences conversation.Preferences) (conversationPreferenceIssue, error) {
	var result conversationPreferenceIssue
	if record.WorkItemID == "" {
		return result, nil
	}
	plan, err := s.loadWorkItemPlan(ctx, scope, record.WorkItemID, "", tracker.UpdateIssue{})
	if err != nil {
		return result, err
	}
	result.plan = plan
	body := conversationAgentOverrideBody(plan.issue.Body, preferences)
	if body == plan.issue.Body {
		return result, nil
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.authorizeWorkItemRecordScope(ctx, reader, scope)
	}); err != nil {
		return result, err
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	plan.issue.Body = body
	plan.issue.Revision++
	plan.issue.UpdatedAt, plan.issue.LastActivityAt = now, now
	plan.fields = []string{"body"}
	result.plan, result.changed = plan, true
	result.fields, err = s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(plan.issue)
	if err != nil {
		return result, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	result.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: record.WorkItemID, Revision: int64(plan.issue.Revision)}, raw)
	if err != nil {
		return result, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: plan.issue.WorkItemID, Type: "issue.edited", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: plan.issue.Revision, Fields: plan.fields}})
	if err != nil {
		return result, err
	}
	result.event, err = content.PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return result, err
	}
	result.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, ID: "issue:" + record.WorkItemID, SourceID: record.WorkItemID, Kind: "issue", Scope: "issue", Revision: int64(plan.issue.Revision), UpdatedAt: now}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
	return result, err
}

func (p conversationPreferenceIssue) lock(ctx context.Context, writer tenantstore.Writer) error {
	if p.plan.current.ID == 0 {
		return nil
	}
	project, err := writer.ProjectRecord(ctx, p.plan.current.ProjectID)
	if err != nil {
		return err
	}
	if !sameWorkItemProjectPlan(p.plan.project, project) {
		return corestore.ErrSerializationFailure
	}
	current, err := writer.LockWorkItem(ctx, p.plan.current.ProjectID, p.plan.current.NativeID)
	if err != nil {
		return err
	}
	if current.Revision != p.plan.current.Revision {
		return nativeConflict(tracker.Revision(current.Revision))
	}
	return nil
}

func (p conversationPreferenceIssue) write(ctx context.Context, writer tenantstore.Writer) error {
	if !p.changed {
		return nil
	}
	if err := writeWorkItemRecord(ctx, writer, p.plan, p.fields); err != nil {
		return err
	}
	source, err := writer.LinkedSource(ctx, p.plan.current.ProjectID, p.plan.current.NativeID)
	if err == nil {
		if source.SnapshotSHA256 != "" {
			source.ExpectedSnapshotSHA256 = new(source.SnapshotSHA256)
		}
		source.BodyOwned = true
		if _, err := writer.UpdateLinkedSource(ctx, source); err != nil {
			return err
		}
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if _, err := writer.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if err := writer.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: p.version.OrganizationID, ProjectID: p.version.ProjectID, WorkItemID: p.version.WorkItemID, Revision: p.version.Revision, VersionSHA256: p.version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(p.plan.issue.Body)}); err != nil {
		return err
	}
	if _, err := writer.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	if err := writer.PutSearch(ctx, p.search, p.plan.current.Revision); err != nil {
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return writer.PutSearch(ctx, p.search, 0)
	}
	return nil
}

// conversationExcerpt bounds a message excerpt to limit runes, the ellipsis
// included, so the listing never returns more than the bound it advertises.
// boundRunes is not reused: it appends its ellipsis beyond the limit.
func conversationExcerpt(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

// References listing.

// conversationWorkItemReference is one "referenced from" entry.
type conversationWorkItemReference struct {
	MessageID      string    `json:"message_id"`
	ConversationID string    `json:"conversation_id"`
	Excerpt        string    `json:"excerpt"`
	CreatedAt      time.Time `json:"created_at"`
}

type conversationWorkItemReferencePage struct {
	References []conversationWorkItemReference `json:"references"`
}

// Settle sweep.

// conversationSettleInterval is how often the sweep runs. It is a variable
// so tests can shorten it.
var conversationSettleInterval = 5 * time.Minute

func (s *Service) readWorkItemReferences(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var stored []conversationstore.WorkItemReference
	var audience string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		principal := ""
		if item.ActorPrincipalID != nil {
			principal = *item.ActorPrincipalID
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: principal}) {
			return nativeNotFound()
		}
		audience, err = conversationReadAudienceRecord(ctx, r, scope.organization, scope.credential)
		if err != nil {
			return err
		}
		owner, ok := r.(conversationstore.WorkItemReferenceReader)
		if !ok {
			return errors.New("conversation reference owner is required")
		}
		stored, err = owner.WorkItemReferences(ctx, scope.project, id, conversationReferencePage)
		return err
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	page := conversationWorkItemReferencePage{References: make([]conversationWorkItemReference, 0, len(stored))}
	for _, reference := range stored {
		if reference.ReadAudience != "" && reference.ReadAudience != audience {
			continue
		}
		if reference.Visibility == conversation.VisibilityPrivate && reference.OwnerPrincipalID != scope.credential.ID {
			continue
		}
		if _, err := s.readPGConversation(ctx, scope, reference.ConversationID); err != nil {
			var denied *nativeError
			if errors.As(err, &denied) && (denied.status == http.StatusNotFound || denied.status == http.StatusForbidden) {
				continue
			}
			return nil, err
		}
		message, err := s.conversations.readPublicationMessage(ctx, scope.organization, reference.ConversationID, reference.MessageID)
		if err != nil {
			return nil, err
		}
		page.References = append(page.References, conversationWorkItemReference{MessageID: reference.MessageID, ConversationID: reference.ConversationID, Excerpt: conversationExcerpt(message.Text, conversationReferenceExcerptRunes), CreatedAt: reference.CreatedAt})
	}
	return json.Marshal(page)
}
