package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	conversationTitleMaxRunes     = 200
	conversationDerivedTitleRunes = 80
	conversationDefaultTitle      = "New chat"
)

// registerConversationAPIRoutes mounts the operator-facing conversation API
// (contract section 5). Hosted sessions authenticate as operator credentials.
//
// The project routes use requireConversationScope rather than
// requireNativeScope: the native middleware turns a hosted read-only grant
// into an opaque 404 on every mutation, while the conversation contract
// distinguishes "cannot see" (404) from "can read but cannot control" (403).
// Write capability is therefore checked per handler by authorizeWrite.
func (s *Service) registerConversationAPIRoutes(e *echo.Echo) {
	scope := s.requireConversationScope()
	e.POST(nativeBase+"/conversations", s.createConversation, scope)
	e.GET(nativeBase+"/conversations", s.listProjectConversations, scope)
	e.GET("/api/v2/organizations/:organization/conversations", s.listOrganizationConversations, s.requireConversationOrganization())
	e.GET(nativeBase+"/conversations/:conversation", s.getConversation, scope)
	e.GET(nativeBase+"/conversations/:conversation/messages", s.listConversationMessages, scope)
	e.GET(nativeBase+"/conversations/:conversation/events", s.streamConversationEvents, scope)
	e.POST(nativeBase+"/conversations/:conversation/commands", s.postConversationCommand, scope)
	e.POST(nativeBase+"/conversations/:conversation/link", s.linkConversation, scope)
	e.PATCH(nativeBase+"/conversations/:conversation", s.patchConversation, scope)
	e.GET(nativeBase+"/work-items/:item/references", s.listWorkItemReferences, scope)
	e.POST(nativeBase+"/conversations/:conversation/attachments", s.uploadConversationAttachment, scope)
	// The download also answers a worker: the runner fetches the files the
	// control named with its own token, so the conversation's read rule
	// governs the bytes the provider sees (decisions section 17.1).
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
			if err := s.requireHostedProject(ctx, s.database.db, scope, false); err != nil {
				return s.nativeAPIError(c, err)
			}
			if err := s.database.authorizeNativeProject(ctx, scope); err != nil {
				return s.nativeAPIError(c, err)
			}
			c.Set("native_scope", scope)
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
// credentials outside the native project base. The organization check is
// the hosted tenant for sessions, and at least one grant in the
// organization for tokens (instance administrators need none).
func (s *Service) requireConversationOrganization() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, status, err := s.authenticateAPIRequest(c)
			if err != nil {
				return c.JSON(status, apiErrorResponse{Code: "unauthorized", Message: "Valid scoped API token is required"})
			}
			if s.config.Hosted != nil && credential.Hosted == nil {
				return s.nativeAPIError(c, nativeNotFound())
			}
			if credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin {
				return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "insufficient_scope", Message: "API token scope does not permit this operation"})
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
	if scope.credential.Hosted != nil {
		if string(scope.organization) != s.config.Hosted.OrganizationID {
			return nativeNotFound()
		}
		return nil
	}
	var count int
	query := "SELECT count(*) FROM organizations WHERE id = ?"
	args := []any{scope.organization}
	if scope.credential.Scope != apiScopeAdmin || scope.credential.NativeOnly {
		query = "SELECT count(*) FROM token_grants WHERE token_id = ? AND organization_id = ?"
		args = []any{scope.credential.ID, scope.organization}
	}
	if err := s.database.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return fmt.Errorf("authorize conversation organization: %w", err)
	}
	if count == 0 {
		return nativeNotFound()
	}
	return nil
}

// transact runs fn inside one transaction with the hub clock and commits
// when fn succeeds. The hub pool holds a single connection, so fn must not
// open another transaction.
func (c *conversationService) transact(ctx context.Context, fn func(tx *sql.Tx, now time.Time) error) (resultErr error) {
	now, err := c.server.database.currentTime()
	if err != nil {
		return err
	}
	tx, err := c.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin conversation transaction: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := fn(tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit conversation transaction: %w", err)
	}
	return nil
}

// requireActorAuthority re-validates the credential inside the mutation
// transaction: live hosted membership and session for sessions, token
// revocation and expiry for API tokens.
func (c *conversationService) requireActorAuthority(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	if scope.credential.Hosted != nil {
		return c.server.recheckHostedMutation(ctx, tx, scope)
	}
	return requireCredentialAuthority(ctx, tx, scope.credential, now)
}

// authorizeManage allows the owner, or any actor with write access, to
// rename a conversation or change its preferences.
func (c *conversationService) authorizeManage(ctx context.Context, query nativeQueryer, scope nativeScope, record conversationRecord) error {
	if err := c.authorizeRead(scope, record); err != nil {
		return err
	}
	if record.OwnerPrincipalID == scope.credential.ID {
		return nil
	}
	return c.authorizeWrite(ctx, query, scope, record)
}

// Request and response shapes.

type conversationFirstMessage struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

type conversationCreateRequest struct {
	// Key makes creation idempotent by actor and key: a retry returns the
	// stored response and a different payload under the same key is
	// idempotency_conflict (decisions section 10.2). first_message.key
	// stays the message command key.
	Key          string                    `json:"key"`
	Title        string                    `json:"title"`
	FirstMessage *conversationFirstMessage `json:"first_message"`
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
// dispatched now or left for later (decisions section 14).
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
// first dispatchable state (decisions section 14).
func resolveConversationNext(project tracker.NativeProject, request conversationLinkRequest) (conversationLinkNextApplied, error) {
	next := conversationLinkNext{}
	if request.Next != nil {
		next = *request.Next
	}
	dispatch := strings.ToLower(strings.TrimSpace(next.Dispatch))
	switch dispatch {
	case "", conversationDispatchNow, conversationDispatchLater:
	default:
		return conversationLinkNextApplied{}, nativeInvalid("next.dispatch must be now or later")
	}
	state := strings.TrimSpace(next.State)
	if state == "" {
		state = strings.TrimSpace(request.Issue.State)
	}
	switch {
	case state != "":
		named, ok := conversationProjectState(project, state)
		if !ok {
			return conversationLinkNextApplied{}, nativeInvalid("next.state must be one of the project's workflow states")
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
		return conversationLinkNextApplied{}, nativeInvalid("next.priority must be the numeric rank 0 to 3")
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
		return 0, nativeInvalid("Query parameter " + name + " must be a non-negative integer")
	}
	return value, nil
}

// conversationQueryTristate reads an optional boolean filter: absent lists
// both groups, true or false restricts to one (decisions section 14).
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
		return false, false, nativeInvalid("Query parameter " + name + " must be true or false")
	}
}

// createConversation implements POST /conversations. The mutation is
// idempotent by the request's top-level key through nativeMutation, which
// also applies the hosted feature and growth checks that every native
// mutation runs (decisions sections 10.2 and 10.10).
func (s *Service) createConversation(c echo.Context) error {
	var request conversationCreateRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	request.Title = strings.TrimSpace(request.Title)
	if utf8.RuneCountInString(request.Title) > conversationTitleMaxRunes {
		return s.nativeAPIError(c, nativeInvalid(fmt.Sprintf("Title must be at most %d characters", conversationTitleMaxRunes)))
	}
	var command conversation.Command
	if request.FirstMessage != nil {
		command = conversation.Command{Key: request.FirstMessage.Key, Kind: conversation.CommandMessage, Text: request.FirstMessage.Text}
		if err := conversation.ValidateCommand(command); err != nil {
			return s.nativeAPIError(c, nativeInvalid(err.Error()))
		}
	}
	service := s.conversations
	// Write capability is checked before the mutation so that a read-only
	// member is refused with the contract's forbidden rather than the
	// hosted mutation guard's opaque not_found, as linkConversation does.
	scope := nativeRequestScope(c)
	fresh := conversationRecord{OrganizationID: scope.organization, ProjectID: scope.project, OwnerPrincipalID: scope.credential.ID, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive}
	if err := service.authorizeWrite(c.Request().Context(), s.database.db, scope, fresh); err != nil {
		return s.nativeAPIError(c, err)
	}
	var record conversationRecord
	notify := false
	err := s.nativeMutationStatus(c, http.StatusCreated, tracker.Mutation{IdempotencyKey: request.Key}, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if err := service.authorizeWrite(ctx, tx, scope, fresh); err != nil {
			return nil, err
		}
		if err := service.requireActorAuthority(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		title := request.Title
		if title == "" {
			title = conversationDefaultTitle
			if request.FirstMessage != nil {
				title = deriveConversationTitle(request.FirstMessage.Text)
			}
		}
		record = conversationRecord{
			ID:               conversation.NewConversationID(),
			OrganizationID:   scope.organization,
			ProjectID:        scope.project,
			OwnerPrincipalID: scope.credential.ID,
			Title:            title,
			Visibility:       conversation.VisibilityPrivate,
			Status:           conversation.StatusActive,
			Execution:        conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now},
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if scope.credential.Hosted != nil {
			record.OwnerSubject = scope.credential.Hosted.Subject
		}
		if err := service.store.createConversation(ctx, tx, &record); err != nil {
			return nil, err
		}
		var receipt *conversation.Receipt
		if request.FirstMessage != nil {
			if _, _, err := service.store.reserveCommand(ctx, tx, record.ID, command.Key, command.Kind, conversation.RequestHash(command), now); err != nil {
				return nil, err
			}
			result, err := service.acceptCommand(ctx, tx, scope, &record, command, now)
			if err != nil {
				service.logStaleExecution(string(command.Kind), record.ID, err)
				return nil, err
			}
			if err := service.recordReceipt(ctx, tx, record.ID, record.Execution.Owner.AttemptID, result, now); err != nil {
				return nil, err
			}
			result.UpdatedAt = now
			receipt = &result
		}
		stored, err := service.store.readConversation(ctx, tx, scope.organization, scope.project, record.ID)
		if err != nil {
			return nil, err
		}
		record, notify = stored, receipt != nil
		return conversationCreatedResponse{Conversation: projectConversation(record), Receipt: receipt}, nil
	})
	if notify {
		service.committed(record)
	}
	return err
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
	records, next, err := s.conversations.store.listConversations(c.Request().Context(), s.database.db, filter)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page := conversationListPage{Conversations: make([]conversationResource, 0, len(records)), NextCursor: conversationOptional(next)}
	for _, record := range records {
		page.Conversations = append(page.Conversations, projectConversation(record))
	}
	return c.JSON(http.StatusOK, page)
}

// listProjectConversations implements GET /conversations for a project.
func (s *Service) listProjectConversations(c echo.Context) error {
	return s.listConversationsPage(c, conversationListQuery{Project: nativeRequestScope(c).project})
}

// listOrganizationConversations implements the organization-level listing:
// the union of the projects the actor can read, most recent activity first.
func (s *Service) listOrganizationConversations(c echo.Context) error {
	scope := nativeRequestScope(c)
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
func (s *Service) readableConversationProjects(ctx context.Context, scope nativeScope) (projects []tracker.ProjectID, every bool, err error) {
	query := "SELECT project_id FROM token_grants WHERE token_id = ? AND organization_id = ?"
	args := []any{scope.credential.ID, scope.organization}
	switch {
	case scope.credential.Hosted != nil:
		query = `SELECT g.project_id FROM hosted_project_grants g JOIN hosted_members m ON m.user_id = g.user_id
WHERE m.user_id = ? AND m.active = 1 AND g.organization_id = ?`
		args = []any{scope.credential.Hosted.Subject, scope.organization}
	case scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly:
		return nil, true, nil
	}
	rows, err := s.database.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list readable projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	projects = []tracker.ProjectID{}
	for rows.Next() {
		var project tracker.ProjectID
		if err := rows.Scan(&project); err != nil {
			return nil, false, fmt.Errorf("list readable projects: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list readable projects: %w", err)
	}
	return projects, false, nil
}

// getConversation implements GET /conversations/:conversation. The record,
// the latest message page and the pending questions are read in one
// transaction so that the returned cursor matches the messages exactly.
func (s *Service) getConversation(c echo.Context) error {
	scope := nativeRequestScope(c)
	service := s.conversations
	ctx := c.Request().Context()
	var snapshot conversationSnapshot
	err := service.transact(ctx, func(tx *sql.Tx, _ time.Time) error {
		record, err := service.loadConversation(ctx, tx, scope, c.Param("conversation"))
		if err != nil {
			return err
		}
		messages, err := service.store.listMessages(ctx, tx, record.ID, 0, conversationMessagePage)
		if err != nil {
			return err
		}
		questions, err := service.store.listSnapshotQuestions(ctx, tx, record.ID, record.Execution.Owner.AttemptID, conversationSnapshotQuestions)
		if err != nil {
			return err
		}
		snapshot = conversationSnapshot{Conversation: projectConversation(record), Messages: projectMessages(messages), Questions: make([]conversation.Question, 0, len(questions)), Cursor: record.EventSeq}
		snapshot.HasMore, _ = conversationOlderCursor(snapshot.Messages)
		for _, question := range questions {
			snapshot.Questions = append(snapshot.Questions, question.Question)
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, snapshot)
}

// listConversationMessages implements GET /conversations/:conversation/messages.
func (s *Service) listConversationMessages(c echo.Context) error {
	scope := nativeRequestScope(c)
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
	ctx := c.Request().Context()
	record, err := s.conversations.loadConversation(ctx, s.database.db, scope, c.Param("conversation"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	messages, err := s.conversations.store.listMessages(ctx, s.database.db, record.ID, before, int(limit))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	page := conversationMessagesPage{Messages: projectMessages(messages)}
	_, page.NextCursor = conversationOlderCursor(page.Messages)
	return c.JSON(http.StatusOK, page)
}

// postConversationCommand implements POST /conversations/:conversation/commands.
func (s *Service) postConversationCommand(c echo.Context) error {
	var command conversation.Command
	if err := decodeAPIJSON(c, &command); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := conversation.ValidateCommand(command); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	scope := nativeRequestScope(c)
	service := s.conversations
	ctx := c.Request().Context()
	var record conversationRecord
	var receipt *conversation.Receipt
	replay := false
	err := service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		record, err = service.loadConversation(ctx, tx, scope, c.Param("conversation"))
		if err != nil {
			return err
		}
		if err := service.authorizeWrite(ctx, tx, scope, record); err != nil {
			return err
		}
		if err := service.requireActorAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		stored, conflict, err := service.store.reserveCommand(ctx, tx, record.ID, command.Key, command.Kind, conversation.RequestHash(command), now)
		if err != nil {
			return err
		}
		if stored != nil {
			if conflict {
				return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict}
			}
			receipt, replay = stored, true
			return nil
		}
		result, err := service.acceptCommand(ctx, tx, scope, &record, command, now)
		if err != nil {
			return err
		}
		if err := service.recordReceipt(ctx, tx, record.ID, record.Execution.Owner.AttemptID, result, now); err != nil {
			return err
		}
		result.UpdatedAt = now
		receipt = &result
		return nil
	})
	if err != nil {
		service.logStaleExecution(string(command.Kind), record.ID, err)
		return s.nativeAPIError(c, err)
	}
	if !replay {
		service.committed(record)
	}
	return c.JSON(http.StatusOK, receipt)
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

// acceptCommand applies a validated, authorized, newly reserved command to
// the conversation inside tx and returns the receipt to store. Errors roll
// the whole command back, including its reservation.
func (c *conversationService) acceptCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, record *conversationRecord, command conversation.Command, now time.Time) (conversation.Receipt, error) {
	receipt := conversation.Receipt{Key: command.Key, Kind: command.Kind, Status: conversation.DeliverySaved}
	// An accepted command is activity: a settled conversation wakes before
	// the command is applied (decisions section 14).
	if err := c.unsettle(ctx, tx, record, now); err != nil {
		return receipt, err
	}
	actor := conversationActorFor(scope)
	linked := record.WorkItemID != ""
	// Whether an attempt was live when the command arrived decides who
	// carries it: a live attempt takes it through the controls poll, and
	// anything else has to wait for the next attempt on the same item. The
	// status is read before the command is applied, because applying it is
	// what moves the execution to waiting_for_runner.
	pending := linked && !conversationExecutionLive(record.Execution.Status)
	switch command.Kind {
	case conversation.CommandMessage:
		message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageText, Text: command.Text, Actor: actor, CommandKey: command.Key}
		switch {
		case !linked:
			// No coordinator answers an unlinked conversation yet, so the
			// message is saved and bounded by the same queue as a worker's.
			if err := c.requireQueueCapacity(ctx, tx, record.ID, command.Kind); err != nil {
				return receipt, err
			}
			message.Delivery = conversation.DeliverySaved
		default:
			if err := conversationRequireOwner(record.Execution.Owner, command.Expected); err != nil {
				return receipt, err
			}
			if err := c.requireQueueCapacity(ctx, tx, record.ID, command.Kind); err != nil {
				return receipt, err
			}
			message.Delivery = conversation.DeliveryQueued
			message.AttemptID = record.Execution.Owner.AttemptID
			message.TurnID = record.Execution.Owner.TurnID
		}
		attachments, err := c.loadCommandAttachments(ctx, tx, scope, *record, command.Attachments)
		if err != nil {
			return receipt, err
		}
		if err := c.appendMessage(ctx, tx, record, &message, now); err != nil {
			return receipt, err
		}
		if err := c.bindMessageAttachments(ctx, tx, *record, &message, attachments); err != nil {
			return receipt, err
		}
		// A message queued on a linked conversation with no live attempt
		// can only be delivered by the next attempt, so it is a request for
		// one.
		if pending {
			if err := requestNativeDispatch(ctx, tx, scope, record.WorkItemID, now); err != nil {
				return receipt, err
			}
		}
		receipt.Status = message.Delivery
		receipt.MessageID = message.ID
	case conversation.CommandAnswer:
		question, err := c.store.readQuestion(ctx, tx, record.ID, command.QuestionID)
		if err != nil {
			return receipt, translateConversationError(err)
		}
		switch question.Status {
		case conversation.QuestionPending:
		case conversation.QuestionSending, conversation.QuestionSent, conversation.QuestionAnswered:
			return receipt, conversationQuestionAnswered()
		default:
			return receipt, conversationStale("The question is no longer open")
		}
		if err := conversationRequireOwner(record.Execution.Owner, conversation.Expected{AttemptID: question.Owner.AttemptID}); err != nil {
			return receipt, err
		}
		if err := conversationRequireOwner(record.Execution.Owner, command.Expected); err != nil {
			return receipt, err
		}
		if err := conversation.ValidateAnswers(question.Question, command.Answers); err != nil {
			return receipt, nativeInvalid(err.Error())
		}
		if err := c.requireQueueCapacity(ctx, tx, record.ID, command.Kind); err != nil {
			return receipt, err
		}
		data, err := marshalNative(command.Answers)
		if err != nil {
			return receipt, fmt.Errorf("encode answers: %w", err)
		}
		message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageAnswer, Text: renderAnswer(question.Question, command.Answers), Data: []byte(data), Delivery: conversation.DeliveryQueued, AttemptID: question.Owner.AttemptID, ThreadID: question.Owner.ThreadID, TurnID: question.Owner.TurnID, Actor: actor, CommandKey: command.Key}
		if err := c.appendMessage(ctx, tx, record, &message, now); err != nil {
			return receipt, err
		}
		question.Status = conversation.QuestionSending
		question.Answers = command.Answers
		question.AnsweredBy = scope.credential.ID
		question.UpdatedAt = now
		if err := c.store.upsertQuestion(ctx, tx, question); err != nil {
			return receipt, err
		}
		if _, err := c.store.appendEvent(ctx, tx, record.ID, conversation.EventQuestionUpdated, question.Question, now); err != nil {
			return receipt, err
		}
		receipt.Status = conversation.DeliveryQueued
		receipt.MessageID = message.ID
		receipt.QuestionID = question.ID
	case conversation.CommandInterrupt:
		if err := conversationRequireOwner(record.Execution.Owner, command.Expected); err != nil {
			return receipt, err
		}
		switch record.Execution.Status {
		case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput:
		default:
			return receipt, conversationStale("No attempt is running")
		}
		if err := c.requireQueueCapacity(ctx, tx, record.ID, command.Kind); err != nil {
			return receipt, err
		}
		message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageInterrupt, Delivery: conversation.DeliveryQueued, AttemptID: record.Execution.Owner.AttemptID, ThreadID: record.Execution.Owner.ThreadID, TurnID: record.Execution.Owner.TurnID, Actor: actor, CommandKey: command.Key}
		if err := c.appendMessage(ctx, tx, record, &message, now); err != nil {
			return receipt, err
		}
		execution := record.Execution
		execution.Status = conversation.ExecutionInterrupting
		if err := c.updateExecution(ctx, tx, record, execution, now); err != nil {
			return receipt, err
		}
		receipt.Status = conversation.DeliveryQueued
		receipt.MessageID = message.ID
	case conversation.CommandContinue:
		if !linked {
			return receipt, conversationUnsupported("Continue requires a linked issue")
		}
		if err := c.requireContinueAttempt(ctx, tx, *record, command); err != nil {
			return receipt, err
		}
		if err := conversationRequireOwner(record.Execution.Owner, command.Expected); err != nil {
			return receipt, err
		}
		switch record.Execution.Status {
		case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput, conversation.ExecutionInterrupting:
			return receipt, conversationStale("An attempt is still running")
		}
		message := conversationMessageRecord{Role: conversation.RoleUser, Kind: conversation.MessageContinue, Text: command.Text, Delivery: conversation.DeliverySaved, Actor: actor, CommandKey: command.Key}
		if err := c.appendMessage(ctx, tx, record, &message, now); err != nil {
			return receipt, err
		}
		if err := requestNativeDispatch(ctx, tx, scope, record.WorkItemID, now); err != nil {
			return receipt, err
		}
		execution := conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: record.Execution.Owner.ThreadID}}
		if err := c.updateExecution(ctx, tx, record, execution, now); err != nil {
			return receipt, err
		}
		receipt.Status = conversation.DeliverySaved
		receipt.MessageID = message.ID
	case conversation.CommandCancel:
		if linked {
			return receipt, conversationUnsupported("Cancel applies to coordinator turns; interrupt the linked attempt instead")
		}
		receipt.Status = conversation.DeliveryRejected
		receipt.Error = &conversation.ReceiptError{Code: "no_active_turn", Message: "No coordinator turn is running"}
	case conversation.CommandRetry:
		retried, err := c.retryMessage(ctx, tx, scope, record, command, now)
		if err != nil {
			return receipt, err
		}
		// A retried message goes back on the ladder as queued; with no live
		// attempt to hand it to, it is the next attempt's to carry
		// (decisions section 10.3).
		if pending && retried.Delivery == conversation.DeliveryQueued {
			if err := requestNativeDispatch(ctx, tx, scope, record.WorkItemID, now); err != nil {
				return receipt, err
			}
		}
		receipt.Status = retried.Delivery
		receipt.MessageID = retried.ID
	default:
		return receipt, conversationUnsupported("Command kind is not supported")
	}
	return receipt, nil
}

// retryMessage re-queues one user message the hub could not establish.
// Nothing is duplicated: the same message id goes back on the ladder and
// the retry key gets its own receipt (decisions section 10.3).
func (c *conversationService) retryMessage(ctx context.Context, tx *sql.Tx, scope nativeScope, record *conversationRecord, command conversation.Command, now time.Time) (conversationMessageRecord, error) {
	messages, err := c.queryMessages(ctx, tx, conversationMessageQuery+"conversation_id = ? AND id = ?", record.ID, command.MessageID)
	if err != nil {
		return conversationMessageRecord{}, err
	}
	if len(messages) == 0 {
		return conversationMessageRecord{}, nativeInvalid("The message to retry is not part of this conversation")
	}
	message := messages[0]
	if message.Role != conversation.RoleUser {
		return conversationMessageRecord{}, nativeInvalid("Only a message you sent can be retried")
	}
	switch message.Delivery {
	case conversation.DeliveryUnknown, conversation.DeliveryFailed, conversation.DeliveryRejected:
	default:
		return conversationMessageRecord{}, nativeInvalid(fmt.Sprintf("A message with delivery %s cannot be retried", message.Delivery))
	}
	if err := c.requireQueueCapacity(ctx, tx, record.ID, command.Kind); err != nil {
		return conversationMessageRecord{}, err
	}
	delivery := conversation.DeliveryQueued
	if record.WorkItemID == "" {
		delivery = conversation.DeliverySaved
	}
	// The generation that could not serve the control is cleared, so the
	// next attempt takes it as a fresh delivery.
	message.Delivery = delivery
	message.AttemptID, message.ThreadID, message.TurnID = "", "", ""
	if err := c.updateMessage(ctx, tx, message, now); err != nil {
		return conversationMessageRecord{}, err
	}
	message.UpdatedAt = now
	return message, nil
}

// requireContinueAttempt enforces that a continue names the attempt it
// follows. A null attempt is only honest for a conversation that never had
// one; otherwise the client is continuing a generation it did not observe
// (decisions section 10.8).
func (c *conversationService) requireContinueAttempt(ctx context.Context, tx *sql.Tx, record conversationRecord, command conversation.Command) error {
	if strings.TrimSpace(command.Expected.AttemptID) != "" {
		return nil
	}
	if record.Execution.Owner.AttemptID != "" {
		return nativeInvalid("expected.attempt_id is required to continue a conversation that has an attempt")
	}
	var attempts int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM conversation_starts WHERE conversation_id = ?", record.ID).Scan(&attempts); err != nil {
		return fmt.Errorf("count conversation attempts: %w", err)
	}
	if attempts > 0 {
		return nativeInvalid("expected.attempt_id is required to continue a conversation that had an attempt")
	}
	return nil
}

// requireQueueCapacity enforces the bounded control queue before anything
// is persisted for the control. A refusal is logged: a conversation whose
// controls stopped draining is otherwise invisible in the log.
func (c *conversationService) requireQueueCapacity(ctx context.Context, tx *sql.Tx, conversationID string, kind conversation.CommandKind) error {
	var queued int
	// Everything the hub still owes a worker or a coordinator counts: a
	// control that was handed out but not acknowledged is as much pending
	// work as one that is still waiting.
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM conversation_messages
WHERE conversation_id = ? AND role = 'user' AND delivery IN (?, ?, ?)`, conversationID,
		conversation.DeliverySaved, conversation.DeliveryQueued, conversation.DeliverySending).Scan(&queued); err != nil {
		return fmt.Errorf("count queued controls: %w", err)
	}
	if queued >= c.config.ControlQueueSize {
		c.logger.Warn("conversation.queue_full", "conversation_id", conversationID, "kind", kind, "queued", queued, "limit", c.config.ControlQueueSize)
		return conversationQueueFull()
	}
	return nil
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
		return invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	service := s.conversations
	ctx := c.Request().Context()
	id := c.Param("conversation")
	// Check visibility and write capability before entering the mutation so
	// that denials carry the contract's codes rather than the generic hosted
	// mutation denial.
	preview, err := service.loadConversation(ctx, s.database.db, scope, id)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := service.authorizeWrite(ctx, s.database.db, scope, preview); err != nil {
		return s.nativeAPIError(c, err)
	}
	var linked conversationRecord
	notify := false
	err = s.nativeMutation(c, tracker.Mutation{IdempotencyKey: request.Key}, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		record, err := service.loadConversation(ctx, tx, scope, id)
		if err != nil {
			return nil, err
		}
		if err := service.authorizeWrite(ctx, tx, scope, record); err != nil {
			return nil, err
		}
		if !request.ShareHistory {
			return nil, conversationShareRequired()
		}
		if record.WorkItemID != "" {
			return nil, conversationAlreadyLinked(record.ID)
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		next, err := resolveConversationNext(project, request)
		if err != nil {
			return nil, err
		}
		body := strings.TrimRight(request.Issue.Description, "\n")
		if body != "" {
			body += "\n\n"
		}
		body += "Conversation: " + record.ID
		// The conversation's explicit preferences travel with the issue from
		// the first revision, so the first runner to claim it already reads
		// them (decisions section 14).
		body = conversationAgentOverrideBody(body, record.Preferences)
		created, err := createNativeIssueTx(ctx, tx, scope, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: request.Key}, Title: strings.TrimSpace(request.Issue.Title), Body: body, State: next.State, Labels: request.Issue.Labels, Priority: next.Priority}, now)
		if err != nil {
			return nil, err
		}
		issue, ok := created.(tracker.NativeIssue)
		if !ok {
			return nil, fmt.Errorf("unexpected issue result %T", created)
		}
		record.WorkItemID = string(issue.WorkItemID)
		linkedAt := now
		record.LinkedAt = &linkedAt
		// Cache the issue summary before the first event is emitted so that
		// conversation.updated already carries the linked work item.
		record.WorkItem = &conversationWorkItem{
			ID: string(issue.WorkItemID), Identifier: fmt.Sprintf("%s#%d", project.Name, issue.Number),
			Title: issue.Title, Lane: issue.State,
		}
		if record.Visibility != conversation.VisibilityShared {
			if err := service.store.recordAudience(ctx, tx, record.ID, scope.credential.ID, record.Visibility, conversation.VisibilityShared, now); err != nil {
				return nil, err
			}
			record.Visibility = conversation.VisibilityShared
		}
		execution := conversation.Execution{Status: conversation.ExecutionWaitingForRunner, Owner: conversation.Owner{ThreadID: record.Execution.Owner.ThreadID}}
		if err := service.updateExecution(ctx, tx, &record, execution, now); err != nil {
			return nil, err
		}
		// The result of the handoff is a durable message, not only the link
		// response: it survives a reload and reaches every other tab.
		result, err := marshalNative(map[string]any{"issue": conversationIssueResult(record)})
		if err != nil {
			return nil, fmt.Errorf("encode issue result: %w", err)
		}
		status := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: fmt.Sprintf("Linked to issue #%d: %s", issue.Number, issue.Title), Data: []byte(result), Delivery: conversation.DeliverySaved, Actor: conversationActorFor(scope), CommandKey: request.Key}
		if err := service.appendMessage(ctx, tx, &record, &status, now); err != nil {
			return nil, err
		}
		record, err = service.store.readConversation(ctx, tx, scope.organization, scope.project, record.ID)
		if err != nil {
			return nil, err
		}
		linked, notify = record, true
		linkedIssue := conversationLinkedIssue{ID: string(issue.WorkItemID), Identifier: fmt.Sprintf("%s#%d", project.Name, issue.Number), NativeIssue: issue}
		return conversationLinkResult{Conversation: projectConversation(record), Issue: linkedIssue, Scheduling: conversationScheduling{Lane: issue.State, RunnerBound: false}, Next: next}, nil
	})
	if notify {
		service.committed(linked)
	}
	return err
}

// saveConversationChange loads, authorizes (owner or write access), applies
// change and saves the conversation, then reports the result.
func (s *Service) saveConversationChange(c echo.Context, change func(ctx context.Context, tx *sql.Tx, scope nativeScope, record *conversationRecord, now time.Time) error) error {
	scope := nativeRequestScope(c)
	service := s.conversations
	ctx := c.Request().Context()
	var record conversationRecord
	err := service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		var err error
		record, err = service.loadConversation(ctx, tx, scope, c.Param("conversation"))
		if err != nil {
			return err
		}
		if err := service.authorizeManage(ctx, tx, scope, record); err != nil {
			return err
		}
		if err := service.requireActorAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		if err := change(ctx, tx, scope, &record, now); err != nil {
			return err
		}
		if err := service.saveConversation(ctx, tx, &record, now); err != nil {
			return err
		}
		record, err = service.store.readConversation(ctx, tx, scope.organization, scope.project, record.ID)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service.committed(record)
	return c.JSON(http.StatusOK, projectConversation(record))
}

// patchConversation implements PATCH /conversations/:conversation. It
// renames a conversation, changes its turn preferences, or both; a request
// that asks for neither is refused rather than silently accepted.
func (s *Service) patchConversation(c echo.Context) error {
	var request conversationPatchRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if request.Title == nil && request.Preferences == nil {
		return s.nativeAPIError(c, nativeInvalid("A patch must change the title, the preferences, or both"))
	}
	title := ""
	if request.Title != nil {
		title = strings.TrimSpace(*request.Title)
		if title == "" || utf8.RuneCountInString(title) > conversationTitleMaxRunes {
			return s.nativeAPIError(c, nativeInvalid(fmt.Sprintf("Title must contain 1 to %d characters", conversationTitleMaxRunes)))
		}
	}
	return s.saveConversationChange(c, func(ctx context.Context, tx *sql.Tx, scope nativeScope, record *conversationRecord, now time.Time) error {
		if request.ProjectID != "" && tracker.ProjectID(request.ProjectID) != record.ProjectID {
			return conversationProjectFixed()
		}
		if request.Title != nil {
			record.Title = title
		}
		if request.Preferences == nil {
			return nil
		}
		preferences := request.Preferences.apply(record.Preferences)
		models, err := s.conversationModelChoices(ctx, tx, record.OrganizationID, []string{string(record.ProjectID)}, now)
		if err != nil {
			return err
		}
		if err := conversation.ValidatePreferences(preferences, conversationModelValidationChoices(models)); err != nil {
			return nativeInvalid(err.Error())
		}
		if preferences == record.Preferences.Normalized() {
			return nil
		}
		record.Preferences = preferences
		// A linked conversation's preferences live in the issue body as
		// well, so a runner that never sees the conversation still honours
		// them (decisions section 14).
		return s.conversations.writeAgentOverride(ctx, tx, scope, *record, now)
	})
}
