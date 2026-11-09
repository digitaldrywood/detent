package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	conversationTitleMaxRunes     = 200
	conversationDerivedTitleRunes = 80
	conversationDefaultTitle      = "New chat"
)

// registerConversationAPIRoutes mounts the operator-facing conversation API
// (the conversation contract). Hosted sessions authenticate as operator credentials.
//
// The project routes use requireConversationScope rather than
// requireNativeScope: the native middleware turns a hosted read-only grant
// into an opaque 404 on every mutation, while the conversation contract
// distinguishes "cannot see" (404) from "can read but cannot control" (403).
// Write capability is therefore checked per handler by authorizeWrite.
func (s *Service) registerConversationAPIRoutes(e *echo.Echo) {
	scope := s.requireConversationScope()
	e.POST(nativeBase+"/conversations", s.createConversation, scope, s.operatorAuthority)
	e.GET(nativeBase+"/conversations", s.listProjectConversations, scope)
	e.GET("/api/v2/organizations/:organization/conversations", s.listOrganizationConversations, s.requireConversationOrganization())
	e.GET(nativeBase+"/conversations/:conversation", s.getConversation, scope)
	e.GET(nativeBase+"/work-items/:item/conversation", s.getWorkItemConversation, scope)
	e.GET(nativeBase+"/conversations/:conversation/messages", s.listConversationMessages, scope)
	e.GET(nativeBase+"/conversations/:conversation/events", s.streamConversationEvents, scope)
	e.POST(nativeBase+"/conversations/:conversation/actions", s.postConversationAction, scope, s.operatorAuthority)
	e.POST(nativeBase+"/conversations/:conversation/commands", s.postConversationCommand, scope, s.operatorAuthority)
	e.POST(nativeBase+"/conversations/:conversation/link", s.linkConversation, scope)
	e.PATCH(nativeBase+"/conversations/:conversation", s.patchConversation, scope)
	e.POST(nativeBase+"/conversations/:conversation/archive", s.archiveConversation, scope)
	e.DELETE(nativeBase+"/conversations/:conversation", s.deleteConversation, scope)
	e.GET(nativeBase+"/work-items/:item/references", s.listWorkItemReferences, scope)
	e.POST(nativeBase+"/conversations/:conversation/attachments", s.uploadConversationAttachment, scope)
	// The download also answers a worker: the runner fetches the files the
	// control named with its own token, so the conversation's read rule
	// governs the bytes the provider sees.
	e.GET(nativeBase+"/conversations/:conversation/attachments/:attachment", s.getConversationAttachment, s.requireConversationScope(apiScopeWorker))
	e.DELETE(nativeBase+"/conversations/:conversation/attachments/:attachment", s.deleteConversationAttachment, scope)
}

// requireConversationScope authenticates an operator or admin credential
// for a project route and requires read access to the project. It mirrors
// requireNativeScope except that hosted write capability is left to the
// handlers so that a read-only member receives 403 instead of 404. Extra
// scopes widen the credential set; the per-handler authorization is
// unchanged, so a wider route is still bounded by the conversation's own
// read rule.
func (s *Service) requireConversationScope(extra ...apiScope) echo.MiddlewareFunc {
	allowed := append([]apiScope{apiScopeOperator, apiScopeAdmin}, extra...)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireAPIScope(allowed...)(func(c echo.Context) error {
			credential, ok := c.Get("hub_api_credential").(apiCredential)
			if !ok {
				return s.nativeAPIError(c, nativeNotFound())
			}
			ctx := c.Request().Context()
			scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project")), credential: credential}

			if err := s.authorizeProjectRecord(ctx, scope, false); err != nil {
				if c.Param("conversation") != "" {
					var denied *nativeError
					if errors.Is(err, accessstore.ErrDenied) || errors.As(err, &denied) && denied.status == http.StatusForbidden {
						return s.nativeAPIError(c, nativeNotFound())
					}
				}
				return s.nativeAPIError(c, err)
			}
			if _, err := s.readProjectRecord(ctx, scope); err != nil {
				return s.nativeAPIError(c, err)
			}
			if item := c.Param("item"); item != "" {
				if _, _, err := s.readWorkItemRecord(ctx, scope, item); err != nil {
					return s.nativeAPIError(c, err)
				}
			}

			c.Set("native_scope", scope)
			if c.Request().Method == http.MethodGet {
				c.SetRequest(c.Request().WithContext(s.archiveRequestContext(c, scope)))
			}
			c.Response().Header().Set("Cache-Control", "no-store")
			if credential.Hosted != nil {
				if err := s.hostedAudit(ctx, credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.project), 0); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			return next(c)
		})
	}
}

// requireConversationOrganization authenticates an operator or admin
// credential for the organization-level listing, which has no :project.
// requireAPIScope cannot be reused: in hosted mode it rejects session
// credentials outside the native project base.
func (s *Service) requireConversationOrganization() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.authenticateAPIRequest(c)
			if err != nil {
				return s.refusedAPIError(c, status, "unauthorized", "Valid scoped API token is required", apiErrorResponse{Code: "unauthorized", Message: "Valid scoped API token is required"})
			}
			if s.config.Hosted != nil && credential.Hosted == nil {
				return s.nativeAPIError(c, nativeNotFound())
			}
			if credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin {
				return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "API token scope does not permit this operation", apiErrorResponse{Code: "insufficient_scope", Message: "API token scope does not permit this operation"})
			}
			ctx := c.Request().Context()
			scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
			if err := s.authorizeConversationOrganization(ctx, scope); err != nil {
				return s.nativeAPIError(c, err)
			}
			c.Set("hub_api_credential", credential)
			c.Set("native_scope", scope)
			c.Response().Header().Set("Cache-Control", "no-store")
			if credential.Hosted != nil {
				if err := s.hostedAudit(ctx, credential.Hosted, "action", c.Request().Method+" "+c.Path(), "", 0); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			return next(c)
		}
	}
}

func (s *Service) authorizeConversationOrganization(ctx context.Context, scope nativeScope) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if scope.credential.Hosted != nil {
		if string(scope.organization) != s.config.Hosted.OrganizationID {
			return nativeNotFound()
		}
		return nil
	}

	if s.config.AccessRepository == nil {
		return nativeNotFound()
	}
	return s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if _, err := r.AccessTenant(ctx); err != nil {
			return err
		}
		if scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly || scope.credential.Scope == apiScopeOperator && !scope.credential.NativeOnly {
			return nil
		}
		grants, err := r.AccessTokenGrants(ctx, scope.credential.ID, "", 1)
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			return nativeNotFound()
		}
		return nil
	})
}

// Request and response shapes.

type conversationFirstMessage struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

type conversationCreateRequest struct {
	// Key makes creation idempotent by actor and key: a retry returns the
	// stored response and a different payload under the same key is
	// idempotency_conflict. first_message.key
	// stays the message command key.
	Key               string                    `json:"key"`
	Title             string                    `json:"title"`
	FirstMessage      *conversationFirstMessage `json:"first_message"`
	SubjectWorkItemID string                    `json:"subject_work_item_id,omitempty"`
}

type conversationCreatedResponse struct {
	Conversation conversationResource  `json:"conversation"`
	Receipt      *conversation.Receipt `json:"receipt,omitempty"`
}

type conversationListPage struct {
	Conversations []conversationResource `json:"conversations"`
	// NextCursor is the opaque keyset cursor of the next page, null on the
	// last page.
	NextCursor *string `json:"next_cursor"`
}

type conversationSnapshot struct {
	Skills       []skills.ProviderSkill        `json:"skills"`
	Compaction   *tracker.EventCompaction      `json:"compaction,omitempty"`
	Conversation conversationResource          `json:"conversation"`
	Messages     []conversationMessageResource `json:"messages"`
	Questions    []conversation.Question       `json:"questions"`
	Cursor       int64                         `json:"cursor"`
	// HasMore reports that older messages exist beyond the returned page.
	HasMore bool `json:"has_more"`
}

type conversationMessagesPage struct {
	Messages []conversationMessageResource `json:"messages"`
	// NextCursor is the seq to pass as before= for the next older page,
	// null when no older messages exist.
	NextCursor *string `json:"next_cursor"`
}

// conversationOlderCursor reports whether messages older than the page
// exist and the before= value that fetches them. Sequence numbers are
// contiguous from 1, so a page whose oldest seq is above 1 has older rows.
func conversationOlderCursor(messages []conversationMessageResource) (bool, *string) {
	if len(messages) == 0 || messages[0].Seq <= 1 {
		return false, nil
	}
	cursor := strconv.FormatInt(messages[0].Seq, 10)
	return true, &cursor
}

// conversationLinkedIssue is the created issue with the client's short
// identity (id, identifier) alongside the full native issue.
type conversationLinkedIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	tracker.NativeIssue
}

// conversationPriority is the native priority rank. The client models a
// priority as a name chosen from the project vocabulary the bootstrap
// payload publishes, which the hub does not publish yet, so both the rank
// and its decimal string are accepted and anything else is a validation
// failure rather than a silently dropped field.
type conversationPriority int

func (p *conversationPriority) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		return nil
	}
	rank, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("priority must be the numeric rank 0 to 3, got %s", string(data))
	}
	*p = conversationPriority(rank)
	return nil
}

// rank returns the priority for the native issue request, nil when unset.
func (p *conversationPriority) rank() *int {
	if p == nil {
		return nil
	}
	value := int(*p)
	return &value
}

type conversationLinkIssue struct {
	Title       string                `json:"title"`
	Description string                `json:"description"`
	State       string                `json:"state,omitempty"`
	Labels      []string              `json:"labels,omitempty"`
	Priority    *conversationPriority `json:"priority,omitempty"`
}

// conversationLinkNext is what the user chose to do with the new issue:
// which state it lands in, what priority it carries and whether it is
// dispatched now or left for later.
type conversationLinkNext struct {
	State    string                `json:"state,omitempty"`
	Priority *conversationPriority `json:"priority,omitempty"`
	Dispatch string                `json:"dispatch,omitempty"`
}

// conversationLinkNextApplied is the next step the hub actually applied,
// echoed on the link response so the client never has to guess.
type conversationLinkNextApplied struct {
	State    string `json:"state"`
	Priority *int   `json:"priority"`
	Dispatch string `json:"dispatch"`
}

type conversationLinkRequest struct {
	Key          string                `json:"key"`
	ShareHistory bool                  `json:"share_history"`
	Issue        conversationLinkIssue `json:"issue"`
	Next         *conversationLinkNext `json:"next,omitempty"`
}

// Dispatch values of the handoff next step.
const (
	conversationDispatchNow   = "now"
	conversationDispatchLater = "later"
	// conversationBacklogState is the lane a "later" handoff prefers: a
	// non-dispatchable, non-terminal state named Backlog.
	conversationBacklogState = "backlog"
)

// conversationBacklogLane reports the project's Backlog state: a
// non-dispatchable, non-terminal state whose name is Backlog, however it is
// cased. A project without one has no place to park work.
func conversationBacklogLane(project tracker.NativeProject) (string, bool) {
	for _, state := range project.States {
		if !state.Terminal && !state.Dispatchable && strings.EqualFold(strings.TrimSpace(state.Name), conversationBacklogState) {
			return state.Name, true
		}
	}
	return "", false
}

// conversationProjectState reports whether name is one of the project's
// states and returns it with the project's own casing.
func conversationProjectState(project tracker.NativeProject, name string) (string, bool) {
	for _, state := range project.States {
		if strings.EqualFold(strings.TrimSpace(state.Name), strings.TrimSpace(name)) {
			return state.Name, true
		}
	}
	return "", false
}

// conversationStateDispatchable reports whether the named state dispatches.
func conversationStateDispatchable(project tracker.NativeProject, name string) bool {
	for _, state := range project.States {
		if strings.EqualFold(strings.TrimSpace(state.Name), strings.TrimSpace(name)) {
			return state.Dispatchable && !state.Terminal
		}
	}
	return false
}

// resolveConversationNext decides the lane and priority of the issue a
// handoff creates. An explicit state wins; otherwise "later" parks the issue
// in Backlog when the project has one and every other case starts it in the
// first dispatchable state.
func resolveConversationNext(project tracker.NativeProject, request conversationLinkRequest) (conversationLinkNextApplied, error) {
	next := conversationLinkNext{}
	if request.Next != nil {
		next = *request.Next
	}
	dispatch := strings.ToLower(strings.TrimSpace(next.Dispatch))
	switch dispatch {
	case "", conversationDispatchNow, conversationDispatchLater:
	default:
		return conversationLinkNextApplied{}, nativePublicInvalid("next.dispatch must be now or later")
	}
	state := strings.TrimSpace(next.State)
	if state == "" {
		state = strings.TrimSpace(request.Issue.State)
	}
	switch {
	case state != "":
		named, ok := conversationProjectState(project, state)
		if !ok {
			return conversationLinkNextApplied{}, nativePublicInvalid("next.state must be one of the project's workflow states")
		}
		state = named
	case dispatch == conversationDispatchLater:
		if backlog, ok := conversationBacklogLane(project); ok {
			state = backlog
		}
	}
	if state == "" {
		first, ok := firstDispatchableState(project)
		if !ok {
			return conversationLinkNextApplied{}, conversationUnsupported("The project has no dispatchable workflow state for a handoff")
		}
		state = first
	}
	priority := next.Priority
	if priority == nil {
		priority = request.Issue.Priority
	}
	if rank := priority.rank(); rank != nil && (*rank < 0 || *rank > 3) {
		return conversationLinkNextApplied{}, nativePublicInvalid("next.priority must be the numeric rank 0 to 3")
	}
	if dispatch == "" {
		// The applied dispatch is read from the lane the issue lands in, so
		// the response is honest about what happened.
		dispatch = conversationDispatchLater
		if conversationStateDispatchable(project, state) {
			dispatch = conversationDispatchNow
		}
	}
	return conversationLinkNextApplied{State: state, Priority: priority.rank(), Dispatch: dispatch}, nil
}

type conversationScheduling struct {
	Lane        string `json:"lane"`
	RunnerBound bool   `json:"runner_bound"`
}

type conversationLinkResult struct {
	Conversation conversationResource        `json:"conversation"`
	Issue        conversationLinkedIssue     `json:"issue"`
	Scheduling   conversationScheduling      `json:"scheduling"`
	Next         conversationLinkNextApplied `json:"next"`
}

// conversationPatchRequest is the PATCH body. Every field is a pointer so
// that "not mentioned" is distinguishable from "set to empty".
type conversationPatchRequest struct {
	Title       *string                         `json:"title"`
	ProjectID   string                          `json:"project_id"`
	Preferences *conversationPreferencesRequest `json:"preferences"`
}

// conversationPreferencesRequest is a partial preference update: an absent
// field keeps the stored value, "auto" returns it to the project default.
type conversationPreferencesRequest struct {
	Model           *string `json:"model"`
	ReasoningEffort *string `json:"reasoning_effort"`
	Access          *string `json:"access"`
}

func (r *conversationPreferencesRequest) apply(current conversation.Preferences) conversation.Preferences {
	preferences := current.Normalized()
	if r == nil {
		return preferences
	}
	if r.Model != nil {
		preferences.Model = *r.Model
	}
	if r.ReasoningEffort != nil {
		preferences.ReasoningEffort = *r.ReasoningEffort
	}
	if r.Access != nil {
		preferences.Access = *r.Access
	}
	return preferences.Normalized()
}

func projectMessages(records []conversationMessageRecord) []conversationMessageResource {
	messages := make([]conversationMessageResource, 0, len(records))
	for _, record := range records {
		messages = append(messages, projectMessage(record))
	}
	return messages
}

// deriveConversationTitle takes the first non-empty line of the first
// message, truncated to conversationDerivedTitleRunes runes.
func deriveConversationTitle(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > conversationDerivedTitleRunes {
			runes := []rune(line)
			line = strings.TrimSpace(string(runes[:conversationDerivedTitleRunes]))
		}
		return line
	}
	return conversationDefaultTitle
}

func conversationQueryInt(c echo.Context, name string) (int64, error) {
	raw := strings.TrimSpace(c.QueryParam(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, nativePublicInvalid("Query parameter " + name + " must be a non-negative integer")
	}
	return value, nil
}

// conversationQueryTristate reads an optional boolean filter: absent lists
// both groups, true or false restricts to one.
// present is false when the parameter was not given.
func conversationQueryTristate(c echo.Context, name string) (value, present bool, err error) {
	switch strings.ToLower(strings.TrimSpace(c.QueryParam(name))) {
	case "":
		return false, false, nil
	case "1", "true", "yes":
		return true, true, nil
	case "0", "false", "no":
		return false, true, nil
	default:
		return false, false, nativePublicInvalid("Query parameter " + name + " must be true or false")
	}
}

// createConversation implements POST /conversations. The mutation is
// idempotent by the request's top-level key through nativeMutation, which
// also applies the hosted feature and growth checks that every native
// mutation runs.
func (s *Service) createConversation(c echo.Context) error {
	var request conversationCreateRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandCreateConversation(c.Request().Context(), nativeRequestScope(c), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

func (s *Service) listConversationsPage(c echo.Context, filter conversationListQuery) error {
	limit, err := conversationQueryInt(c, "limit")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	scope := nativeRequestScope(c)
	filter.Organization = scope.organization
	filter.Principal = scope.credential.ID
	filter.Limit = int(limit)
	filter.Cursor = strings.TrimSpace(c.QueryParam("cursor"))
	settled, present, err := conversationQueryTristate(c, "settled")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if present {
		filter.Settled = &settled
	}
	filter.Title = strings.TrimSpace(c.QueryParam("q"))
	filter.SubjectWorkItemID = strings.TrimSpace(c.QueryParam("subject_work_item_id"))
	value, err := s.readConversationsPage(c.Request().Context(), scope, filter)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// listProjectConversations implements GET /conversations for a project.
func (s *Service) listProjectConversations(c echo.Context) error {
	return s.listConversationsPage(c, conversationListQuery{Project: nativeRequestScope(c).project})
}

// listOrganizationConversations implements the organization-level listing:
// the union of the projects the actor can read, most recent activity first.
func (s *Service) listOrganizationConversations(c echo.Context) error {
	scope := nativeRequestScope(c)
	if scope.credential.Hosted == nil && scope.credential.EntryKey == nil {
		return s.listConversationsPage(c, conversationListQuery{})
	}

	projects, every, err := s.readableConversationProjects(c.Request().Context(), scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	filter := conversationListQuery{}
	if !every {
		// A non-nil slice, empty when nothing is readable, so the query
		// restricts rather than listing the whole organization.
		filter.Projects = projects
	}
	return s.listConversationsPage(c, filter)
}

// readableConversationProjects returns the projects the actor can read in
// the organization; every is true when all projects are readable (instance
// administrators).
func (s *Service) readableConversationProjects(_ context.Context, scope nativeScope) (projects []tracker.ProjectID, every bool, err error) {
	if scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly {
		return nil, true, nil
	}
	return []tracker.ProjectID{}, false, nil
}

func (s *Service) getWorkItemConversation(c echo.Context) error {
	if c.QueryParam("metadata") == "true" {

		scope := nativeRequestScope(c)
		var record conversationRecord
		err := s.config.TenantRepository.ReadTenant(c.Request().Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			stored, err := reader.ConversationByWorkItem(c.Request().Context(), scope.project, c.Param("item"))
			if err != nil {
				return err
			}
			record = conversationRecordFromStore(stored)
			return nil
		})
		if err != nil {
			return s.nativeAPIError(c, translateConversationError(err))
		}
		record, err = s.readPGConversation(c.Request().Context(), scope, record.ID)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, conversationSnapshot{Skills: []skills.ProviderSkill{}, Conversation: projectConversation(record), Messages: []conversationMessageResource{}, Questions: []conversation.Question{}, Cursor: record.EventSeq})
	}
	value, err := s.readWorkItemConversationSnapshot(c.Request().Context(), nativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

func (s *Service) readWorkItemConversationSnapshot(ctx context.Context, scope nativeScope, item string) (json.RawMessage, error) {
	return s.readConversationSnapshotFor(ctx, scope, "", item, false)
}

// getConversation implements GET /conversations/:conversation. The record,
// the latest message page and the pending questions are read in one
// transaction so that the returned cursor matches the messages exactly.
func (s *Service) getConversation(c echo.Context) error {
	value, err := s.readConversationSnapshot(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, value)
}

// listConversationMessages implements GET /conversations/:conversation/messages.
func (s *Service) listConversationMessages(c echo.Context) error {
	before, err := conversationQueryInt(c, "before")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limit, err := conversationQueryInt(c, "limit")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if limit == 0 {
		limit = conversationMessagePage
	}
	value, err := s.readConversationMessages(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), before, int(limit))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, value)
}

// postConversationCommand implements POST /conversations/:conversation/commands.
func (s *Service) postConversationCommand(c echo.Context) error {
	var command conversation.Command
	if err := decodeAPIJSON(c, &command); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandPostConversation(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), command)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// conversationExecutionLive reports whether an attempt is bound and can be
// handed a control directly, rather than the control having to wait for the
// next attempt on the item.
func conversationExecutionLive(status conversation.ExecutionStatus) bool {
	switch status {
	case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput, conversation.ExecutionInterrupting:
		return true
	default:
		return false
	}
}

// renderAnswer produces the short transcript text of an answer message.
func renderAnswer(question conversation.Question, answers map[string][]string) string {
	for _, prompt := range question.Prompts {
		if values := answers[prompt.ID]; len(values) > 0 {
			return "Answered: " + strings.Join(values, ", ")
		}
	}
	return "Answered"
}

// linkConversation implements POST /conversations/:conversation/link. The
// mutation is idempotent by key through nativeMutation; the issue is created
// with createNativeIssueTx in the same transaction as the link.
func (s *Service) linkConversation(c echo.Context) error {
	var request conversationLinkRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandLinkConversation(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

// saveConversationChange loads, authorizes (owner or write access), applies
// change and saves the conversation, then reports the result.

// patchConversation implements PATCH /conversations/:conversation. It
// renames a conversation, changes its turn preferences, or both; a request
// that asks for neither is refused rather than silently accepted.
func (s *Service) patchConversation(c echo.Context) error {
	var request conversationPatchRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	value, err := s.commandPatchConversation(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

func (s *Service) commandCreateConversation(ctx context.Context, scope nativeScope, request conversationCreateRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.Key) == "" || len(request.Key) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	request.Title = strings.TrimSpace(request.Title)
	if utf8.RuneCountInString(request.Title) > conversationTitleMaxRunes {
		return nil, nativePublicInvalid(fmt.Sprintf("Title must be at most %d characters", conversationTitleMaxRunes))
	}
	var command conversation.Command
	if request.FirstMessage != nil {
		command = conversation.Command{Key: request.FirstMessage.Key, Kind: conversation.CommandMessage, Text: request.FirstMessage.Text}
		if err := conversation.ValidateCommand(command); err != nil {
			return nil, nativeInvalid(err.Error())
		}
	}
	ctx, err := s.bindCoordinatorConnection(ctx, command.Key)
	if err != nil {
		return nil, err
	}
	service := s.conversations
	// Write capability is checked before the mutation so that a read-only
	// member is refused with the contract's forbidden rather than the
	// hosted mutation guard's opaque not_found, as linkConversation does.

	if err := s.authorizeConversationScopePG(ctx, scope, true); err != nil {
		return nil, err
	}

	now := s.config.now()

	if err := s.requireConversationFeaturePG(ctx, scope.organization, "collaboration", now); err != nil {
		return nil, err
	}

	title := request.Title
	if title == "" {
		title = conversationDefaultTitle
		if request.FirstMessage != nil {
			title = deriveConversationTitle(request.FirstMessage.Text)
		}
	}
	if request.SubjectWorkItemID != "" {

		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			item, err := reader.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(request.SubjectWorkItemID))
			if err != nil {
				return err
			}
			title = boundRunes(fmt.Sprintf("#%d · %s", item.Number, title), conversationTitleMaxRunes)
			return nil
		})
		if err != nil {
			return nil, translateConversationError(err)
		}

	}

	if command.Key != "" {
		if err := conversation.ValidateCommand(command); err != nil {
			return nil, nativeInvalid(err.Error())
		}
	}

	prepared := newConversationArchiveReads(ctx)

	ctx = prepared
	freshRecord := conversationRecord{ID: conversation.NewConversationID(), SubjectWorkItemID: request.SubjectWorkItemID, OrganizationID: scope.organization, ProjectID: scope.project, OwnerPrincipalID: scope.credential.ID, Origin: conversationOriginUser, Title: title, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now, Revision: 1}
	if scope.credential.Hosted != nil {
		freshRecord.OwnerSubject = scope.credential.Hosted.Subject
	}
	plan := &conversationPublicationPlan{service: service, record: freshRecord, now: now}
	if err := plan.prepareRecord(ctx, freshRecord); err != nil {
		return nil, err
	}
	if request.FirstMessage != nil {

		message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageText, Text: command.Text, Data: coordinatorConnectionData(ctx), Delivery: conversation.DeliverySaved, Actor: conversationActorFor(scope), CommandKey: command.Key, CreatedAt: now, UpdatedAt: now}
		if err := preparePGConversationMessage(ctx, plan, &message); err != nil {
			return nil, err
		}

	}
	ctx = plan.context(ctx)

	return s.commandCreateConversationPG(ctx, scope, request, command, plan.record, plan)
}

func (s *Service) commandPostConversation(ctx context.Context, scope nativeScope, id string, command conversation.Command) (json.RawMessage, error) {
	if err := conversation.ValidateCommand(command); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	ctx, bindErr := s.bindCoordinatorConnection(ctx, command.Key)
	if bindErr != nil {
		return nil, bindErr
	}
	ctx = newConversationArchiveReads(ctx)

	return s.commandPostConversationPG(ctx, scope, id, command)
}

func conversationLinkedIssueRequest(record conversationRecord, request conversationLinkRequest, next conversationLinkNextApplied) tracker.CreateIssue {
	body := strings.TrimRight(request.Issue.Description, "\n")
	if body != "" {
		body += "\n\n"
	}
	body += "Conversation: " + record.ID
	body = conversationAgentOverrideBody(body, record.Preferences)
	return tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: request.Key}, Title: strings.TrimSpace(request.Issue.Title), Body: body, State: next.State, Labels: request.Issue.Labels, Priority: next.Priority}
}

func (s *Service) commandLinkConversation(ctx context.Context, scope nativeScope, id string, request conversationLinkRequest) (json.RawMessage, error) {

	return s.commandLinkConversationPG(ctx, scope, id, request)
}

func (s *Service) readConversationSnapshot(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	return s.readConversationSnapshotFor(ctx, scope, id, "", false)
}

type conversationPageRead struct {
	record     conversationRecord
	messages   []conversationMessageRecord
	contents   *conversationReadBatch
	compaction tracker.EventCompaction
}

func (s *Service) captureConversationPage(ctx context.Context, scope nativeScope, id, item string, before int64, limit int) (conversationPageRead, error) {

	if item != "" {
		var record conversationRecord
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			stored, err := reader.ConversationByWorkItem(ctx, scope.project, item)
			if err != nil {
				return err
			}
			record = conversationRecordFromStore(stored)
			return nil
		})
		if err != nil {
			return conversationPageRead{}, translateConversationError(err)
		}
		id = record.ID
	}
	return s.readPGConversationPage(ctx, scope, id, before, limit)
}

func (s *Service) readConversationSnapshotFor(ctx context.Context, scope nativeScope, id, item string, diagnoseMissing bool) (json.RawMessage, error) {
	ctx = withConversationArchiveReads(s.archiveContext(ctx, scope, false))
	if item != "" {
		var record conversationRecord
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			stored, err := reader.ConversationByWorkItem(ctx, scope.project, item)
			if err != nil {
				return err
			}
			record = conversationRecordFromStore(stored)
			return nil
		})
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) && diagnoseMissing {
				return nil, conversationUnsupported("Missing conversation diagnostics require the attachments migration")
			}
			return nil, translateConversationError(err)
		}
		id = record.ID
	}
	page, err := s.captureConversationPage(ctx, scope, id, item, 0, conversationMessagePage)
	if err != nil {
		return nil, err
	}
	store := s.conversations.store
	if page.contents != nil {
		if err := page.contents.hydrateRecord(ctx, store, &page.record); err != nil {
			return nil, err
		}
		if err := page.contents.hydrateMessages(ctx, store, page.messages); err != nil {
			return nil, err
		}
	}
	questions, skills, err := s.readConversationSnapshotDetails(ctx, scope, page.record)
	if err != nil {
		return nil, err
	}
	snapshot := conversationSnapshot{Conversation: projectConversation(page.record), Messages: projectMessages(page.messages), Questions: make([]conversation.Question, 0, len(questions)), Skills: skills, Cursor: page.record.EventSeq}
	if page.compaction.RemovedEvents > 0 {
		snapshot.Compaction = &page.compaction
	}
	snapshot.HasMore, _ = conversationOlderCursor(snapshot.Messages)
	for _, question := range questions {
		snapshot.Questions = append(snapshot.Questions, question.Question)
	}
	snapshot.Conversation.WebURL, err = s.canonicalBrowserLink(ctx, "/chat/c/"+url.PathEscape(snapshot.Conversation.ID)+"?project_id="+url.QueryEscape(string(snapshot.Conversation.ProjectID)))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return withNativeArchive(raw, nativeArchiveDescriptor(ctx))
}

func (s *Service) readConversationSnapshotDetails(ctx context.Context, scope nativeScope, record conversationRecord) ([]conversationQuestionRecord, []skills.ProviderSkill, error) {

	var questions []conversationQuestionRecord
	contentReferences := map[string]conversationstore.ContentObject{}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		stored, err := reader.Questions(ctx, record.ID, record.Execution.Owner.AttemptID, conversationSnapshotQuestions)
		if err != nil {
			return err
		}
		for _, question := range stored {
			if question.ContentReference != "" {
				object, err := reader.ConversationContent(ctx, question.ContentReference)
				if err != nil {
					return err
				}
				contentReferences[question.ContentReference] = object
			}
			questions = append(questions, conversationQuestionRecord{Question: question.Question, RequestID: question.RequestID, ContentID: question.ContentReference})
		}
		return nil
	})
	if err == nil {
		for index := range questions {
			question := &questions[index]
			if question.ContentID == "" {
				continue
			}
			object := contentReferences[question.ContentID]
			var payload conversationContent
			if err := s.readConversationObject(ctx, string(scope.organization), object.Reference, &payload); err != nil {
				return nil, nil, err
			}
			var content conversationQuestionContent
			if err := json.Unmarshal(payload.Data, &content); err != nil {
				return nil, nil, fmt.Errorf("decode question content: %w", err)
			}
			question.Prompts, question.Answers = content.Prompts, content.Answers
		}
	}
	return questions, []skills.ProviderSkill{}, translateConversationError(err)
}

func (s *Service) commandPatchConversation(ctx context.Context, scope nativeScope, id string, request conversationPatchRequest) (json.RawMessage, error) {
	if request.Title == nil && request.Preferences == nil {
		return nil, nativePublicInvalid("A patch must change the title, the preferences, or both")
	}
	title := ""
	if request.Title != nil {
		title = strings.TrimSpace(*request.Title)
		if title == "" || utf8.RuneCountInString(title) > conversationTitleMaxRunes {
			return nil, nativePublicInvalid(fmt.Sprintf("Title must contain 1 to %d characters", conversationTitleMaxRunes))
		}
	}

	return s.commandPatchConversationPG(ctx, scope, id, request, title)
}

func (s *Service) readConversationMessages(ctx context.Context, scope nativeScope, id string, before int64, limit int) (json.RawMessage, error) {
	ctx = withConversationArchiveReads(s.archiveContext(ctx, scope, false))
	captured, err := s.captureConversationPage(ctx, scope, id, "", before, limit)
	if err != nil {
		return nil, err
	}
	if captured.contents != nil {
		if err := captured.contents.hydrateMessages(ctx, s.conversations.store, captured.messages); err != nil {
			return nil, err
		}
	}
	page := conversationMessagesPage{Messages: projectMessages(captured.messages)}
	_, page.NextCursor = conversationOlderCursor(page.Messages)
	raw, err := json.Marshal(page)
	if err != nil {
		return nil, err
	}
	return withNativeArchive(raw, nativeArchiveDescriptor(ctx))
}

func (s *Service) readConversationsPage(ctx context.Context, scope nativeScope, filter conversationListQuery) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	filter.Organization = scope.organization
	filter.Principal = scope.credential.ID

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > corestore.MaxPageSize {
		limit = corestore.MaxPageSize
	}
	var page conversationstore.ConversationPage
	var records []conversationRecord
	contentObjects := map[string]conversationstore.ContentObject{}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		var err error
		grantCredential := ""
		if scope.project == "" && scope.credential.Hosted == nil && scope.credential.EntryKey == nil {
			grantCredential = scope.credential.ID
		}
		page, err = reader.Conversations(ctx, conversationstore.ConversationQuery{
			ProjectID: filter.Project, ProjectIDs: filter.Projects, PrincipalID: filter.Principal, GrantCredentialID: grantCredential,
			Cursor: filter.Cursor, Limit: limit, Settled: filter.Settled, Title: filter.Title,
			SubjectWorkItemID: filter.SubjectWorkItemID,
		})
		if err != nil {
			if errors.Is(err, corestore.ErrInvalidCursor) {
				return nativePublicInvalid("Invalid conversation cursor")
			}
			return err
		}
		projectCache := map[tracker.ProjectID]corestore.Project{}
		for _, stored := range page.Items {
			record := conversationRecordFromStore(stored)
			if record.ReadAudience != "" {
				return nativeNotFound()
			}
			if err := authorizeConversationLinkedItemPG(ctx, reader, scope, record); err != nil {
				var denied *nativeError
				if errors.As(err, &denied) && denied.status == http.StatusNotFound {
					continue
				}
				return err
			}
			if stored.ContentReference != "" {
				object, err := reader.ConversationContent(ctx, stored.ContentReference)
				if err != nil {
					return err
				}
				contentObjects[record.ID] = object
			}
			if record.WorkItemID != "" {
				item, err := reader.WorkItem(ctx, record.ProjectID, tracker.NativeWorkItemID(record.WorkItemID))
				if err != nil {
					if errors.Is(err, corestore.ErrNotFound) {
						continue
					}
					return err
				}
				project, ok := projectCache[record.ProjectID]
				if !ok {
					project, err = reader.Project(ctx, record.ProjectID)
					if err != nil {
						return err
					}
					projectCache[record.ProjectID] = project
				}
				record.WorkItem = &conversationWorkItem{ID: string(item.NativeID), Identifier: fmt.Sprintf("%s#%d", project.Name, item.Number), Title: item.Title}
				if item.WorkflowStateName != nil {
					record.WorkItem.Lane = *item.WorkflowStateName
				}
			}
			if record.OrganizationID != scope.organization || (record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.credential.ID) {
				continue
			}
			records = append(records, record)
		}
		return nil
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	for index := range records {
		record := &records[index]
		object, ok := contentObjects[record.ID]
		if !ok {
			continue
		}
		var payload conversationContent
		if err := s.readConversationObject(ctx, string(scope.organization), object.Reference, &payload); err != nil {
			return nil, err
		}
		var content conversationRecordContent
		if err := json.Unmarshal(payload.Data, &content); err != nil {
			return nil, fmt.Errorf("decode conversation content: %w", err)
		}
		record.Execution, record.Preferences = content.Execution, content.Preferences.Normalized()
	}
	result := conversationListPage{Conversations: make([]conversationResource, 0, len(records)), NextCursor: conversationOptional(page.Next)}
	for _, record := range records {
		resource := projectConversation(record)
		resource.WebURL, err = s.canonicalBrowserLink(ctx, conversationCursorURL(record))
		if err != nil {
			return nil, err
		}
		result.Conversations = append(result.Conversations, resource)
	}
	return json.Marshal(result)
}
