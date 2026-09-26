package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentoverride"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Turn preferences, references and the settle sweep (decisions section 14).

// conversationReferenceExcerptRunes bounds the message excerpt a
// "referenced from" entry carries, so the listing never republishes a whole
// conversation to a reader who may not open it.
const conversationReferenceExcerptRunes = 200

// conversationModel is one model a conversation may select, as the hub
// assembled it from the enrolled runners' provider reports.
//
// The identifier is what capacity and dispatch match against; everything else
// is what a runner's backend catalog said about it (decisions section 14), and
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
func (s *Service) conversationModelChoices(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, projects []string, now time.Time) ([]conversationModel, error) {
	models, err := reportedProviderModels(ctx, query, organization, projects, now)
	if err != nil {
		return nil, err
	}
	if configured := s.conversationDefaultModel(); configured != "" &&
		!slices.ContainsFunc(models, func(model conversationModel) bool { return model.ID == configured }) {
		models = append(models, conversationModel{ID: configured, Label: configured})
	}
	return models, nil
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
func reportedProviderModels(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, projects []string, now time.Time) ([]conversationModel, error) {
	statement := `SELECT DISTINCT r.provider_reports_json FROM runner_identities r
JOIN api_tokens t ON t.id = r.token_id
WHERE r.organization_id = ? AND r.state = 'active' AND t.revoked_at IS NULL`
	args := []any{organization}
	if projects != nil {
		if len(projects) == 0 {
			return []conversationModel{}, nil
		}
		encoded, err := json.Marshal(projects)
		if err != nil {
			return nil, fmt.Errorf("encode project list: %w", err)
		}
		statement += ` AND EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id = r.token_id AND g.organization_id = r.organization_id
 AND g.project_id IN (SELECT value FROM json_each(?)))`
		args = append(args, string(encoded))
	}
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list runner provider reports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	models := []conversationModel{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan runner provider reports: %w", err)
		}
		var reports []providercapacity.Report
		if err := json.Unmarshal([]byte(raw), &reports); err != nil {
			return nil, fmt.Errorf("decode runner provider reports: %w", err)
		}
		for _, report := range reports {
			if !reportFresh(report, now) {
				continue
			}
			for _, model := range report.Models {
				model = strings.TrimSpace(model)
				if model == "" {
					continue
				}
				if _, duplicate := seen[model]; duplicate {
					continue
				}
				seen[model] = struct{}{}
				models = append(models, conversationModelFromReport(report, model))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runner provider reports: %w", err)
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
// project's configured default", which is exactly an absent field.
func conversationAgentOverride(preferences conversation.Preferences) agentoverride.Override {
	return agentoverride.Override{Model: preferences.ModelValue(), Effort: preferences.EffortValue()}
}

// conversationAgentOverrideBody returns body carrying the conversation's
// preferences as a detent-agent block.
func conversationAgentOverrideBody(body string, preferences conversation.Preferences) string {
	return agentoverride.ApplyToIssueBody(body, conversationAgentOverride(preferences))
}

// writeAgentOverride keeps a linked conversation's issue body in step with
// its preferences. The issue is read and persisted inside the caller's
// transaction, so the revision the update bumps is the one it read; a body
// that already says the right thing is left alone, which is what makes a
// second identical change write nothing.
func (c *conversationService) writeAgentOverride(ctx context.Context, tx *sql.Tx, scope nativeScope, record conversationRecord, now time.Time) error {
	if record.WorkItemID == "" {
		return nil
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, record.WorkItemID)
	if err != nil {
		return fmt.Errorf("read linked issue for preferences: %w", err)
	}
	body := conversationAgentOverrideBody(issue.Body, record.Preferences)
	if body == issue.Body {
		return nil
	}
	issue.Body = body
	if _, err := persistNativeIssue(ctx, tx, scope, issue, "issue.edited",
		tracker.CollaborationData{Fields: []string{"body"}}, now); err != nil {
		return fmt.Errorf("write conversation preferences to issue: %w", err)
	}
	c.logger.Info("conversation.preferences_written",
		"conversation_id", record.ID, "work_item_id", record.WorkItemID,
		"model", record.Preferences.Model, "reasoning_effort", record.Preferences.ReasoningEffort)
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

// listWorkItemReferences implements GET /work-items/:item/references. The
// scope middleware has already proved the reader can read the project; the
// issue read proves the issue exists in it, and the conversation read rule
// drops entries from private chats the reader does not own.
func (s *Service) listWorkItemReferences(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	if _, _, err := readNativeIssue(ctx, s.database.db, scope, c.Param("item")); err != nil {
		return s.nativeAPIError(c, translateConversationError(err))
	}
	stored, err := s.conversations.store.listWorkItemReferences(ctx, s.database.db, scope.organization, scope.project, c.Param("item"), conversationReferencePage)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page := conversationWorkItemReferencePage{References: make([]conversationWorkItemReference, 0, len(stored))}
	for _, reference := range stored {
		if reference.Visibility == conversation.VisibilityPrivate && reference.OwnerPrincipalID != scope.credential.ID {
			continue
		}
		page.References = append(page.References, conversationWorkItemReference{
			MessageID: reference.MessageID, ConversationID: reference.ConversationID,
			Excerpt: conversationExcerpt(reference.Excerpt, conversationReferenceExcerptRunes), CreatedAt: reference.CreatedAt,
		})
	}
	return c.JSON(http.StatusOK, page)
}

// Settle sweep.

// conversationSettleInterval is how often the sweep runs. It is a variable
// so tests can shorten it.
var conversationSettleInterval = 5 * time.Minute

// settleIdleConversations settles every active conversation whose execution
// has ended or never started and whose last activity is older than the
// settle window. It returns how many settled.
func (c *conversationService) settleIdleConversations(ctx context.Context, now time.Time) (int, error) {
	before := now.Add(-c.config.SettleWindow)
	candidates, err := c.store.listSettleCandidates(ctx, c.store.db, before, conversationSettleBatch)
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, candidate := range candidates {
		changed, err := c.settleConversation(ctx, candidate.ID, before)
		if err != nil {
			return settled, err
		}
		if changed {
			settled++
		}
	}
	return settled, nil
}

// settleConversation settles one conversation, re-reading it inside the
// transaction so a message that arrived since the sweep listed it keeps the
// conversation active.
func (c *conversationService) settleConversation(ctx context.Context, id string, before time.Time) (bool, error) {
	changed := false
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := c.store.readConversationByID(ctx, tx, id)
		if err != nil {
			return err
		}
		// Execution.Settled is a different notion: it records that a bound
		// worker's unbind was applied. What settles a conversation is the
		// execution having ended or never started.
		finished := record.Execution.Status.Terminal() || record.Execution.Status == conversation.ExecutionIdle
		if record.Status != conversation.StatusActive || !finished {
			return nil
		}
		activity := record.UpdatedAt
		if record.LastMessageAt != nil {
			activity = *record.LastMessageAt
		}
		if !activity.Before(before) {
			return nil
		}
		record.Status = conversation.StatusSettled
		at := now
		record.SettledAt = &at
		if err := c.saveConversation(ctx, tx, &record, now); err != nil {
			return err
		}
		changed = true
		c.logger.Info("conversation.settled", "conversation_id", record.ID, "last_activity", activity)
		return nil
	})
	if err != nil {
		return false, err
	}
	if changed {
		c.broker.notify(id)
	}
	return changed, nil
}
