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
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	conversationTitleMaxRunes     = 200
	conversationDerivedTitleRunes = 80
	conversationDefaultTitle      = "New chat"
)

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
	Skills          []skills.ProviderSkill        `json:"skills"`
	SkillsTruncated bool                          `json:"skills_truncated,omitempty"`
	Compaction      *tracker.EventCompaction      `json:"compaction,omitempty"`
	Conversation    conversationResource          `json:"conversation"`
	Messages        []conversationMessageResource `json:"messages"`
	Questions       []conversation.Question       `json:"questions"`
	Cursor          int64                         `json:"cursor"`
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

// readableConversationProjects returns the projects the actor can read in
// the organization; every is true when all projects are readable (instance
// administrators).
func (s *Service) readableConversationProjects(ctx context.Context, scope nativeScope) (projects []tracker.ProjectID, every bool, err error) {
	if scope.credential.Scope == apiScopeAdmin && !scope.credential.NativeOnly {
		return nil, true, nil
	}
	if s.config.TenantRepository == nil {
		return []tracker.ProjectID{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	const pageSize = 200
	projects = []tracker.ProjectID{}
	after := tracker.ProjectID("")
	for {
		var count int
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			pageReader, ok := reader.(accessstore.ProjectAccessPageReader)
			if !ok {
				return ErrProjectParticipants
			}
			rows, err := pageReader.ProjectAccessPage(ctx, projectAccessFilter(scope.credential, "", after, pageSize))
			if err != nil {
				return err
			}
			count = min(len(rows), pageSize)
			if count > 0 {
				after = rows[count-1].ProjectID
			}
			granted, err := s.grantedAccessProjects(ctx, reader, scope.organization, scope.credential, rows[:count])
			if err != nil {
				return err
			}
			for _, row := range granted {
				projects = append(projects, row.ProjectID)
			}
			return nil
		})
		if err != nil {
			return nil, false, fmt.Errorf("list readable conversation projects: %w", err)
		}
		if count < pageSize {
			return projects, false, nil
		}
	}
}

func (s *Service) readWorkItemConversationSnapshot(ctx context.Context, scope nativeScope, item string) (json.RawMessage, error) {
	return s.readConversationSnapshotFor(ctx, scope, "", item, false)
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

// saveConversationChange loads, authorizes (owner or write access), applies
// change and saves the conversation, then reports the result.

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

	now := s.config.now().UTC().Truncate(time.Microsecond)

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

	value, err := s.commandPostConversationPG(ctx, scope, id, command)
	if err != nil {
		return nil, conversationStaleExecutionError(string(command.Kind), id, err)
	}
	return value, nil
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
				runtime, err := s.readNativeRuntime(ctx, scope, item, "")
				if err != nil {
					return nil, err
				}
				return json.Marshal(struct {
					Status       string                        `json:"status"`
					Conversation *conversationResource         `json:"conversation"`
					Runtime      tracker.NativeRuntimeEvidence `json:"runtime"`
				}{Status: "unavailable", Runtime: runtime})
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
	questions, skills, skillsTruncated, err := s.readConversationSnapshotDetails(ctx, scope, page.record)
	if err != nil {
		return nil, err
	}
	snapshot := conversationSnapshot{Conversation: projectConversation(page.record), Messages: projectMessages(page.messages), Questions: make([]conversation.Question, 0, len(questions)), Skills: skills, SkillsTruncated: skillsTruncated, Cursor: page.record.EventSeq}
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

func (s *Service) readConversationSnapshotDetails(ctx context.Context, scope nativeScope, record conversationRecord) ([]conversationQuestionRecord, []skills.ProviderSkill, bool, error) {

	var questions []conversationQuestionRecord
	projectSkills := []skills.ProviderSkill{}
	skillsTruncated := false
	contentReferences := map[string]conversationstore.ContentObject{}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		reports, truncated, err := reader.RunnerProjectSkills(ctx, record.ProjectID, record.Execution.Owner.RunnerID, s.config.now().UTC(), runnerstore.MaxProjectSkills)
		skillsTruncated = truncated
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, report := range reports {
			name := strings.ToLower(report.Skill.Name)
			if !seen[name] {
				seen[name] = true
				projectSkills = append(projectSkills, report.Skill)
			}
		}
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
				return nil, nil, false, err
			}
			var content conversationQuestionContent
			if err := json.Unmarshal(payload.Data, &content); err != nil {
				return nil, nil, false, fmt.Errorf("decode question content: %w", err)
			}
			question.Prompts, question.Answers = content.Prompts, content.Answers
		}
	}
	return questions, projectSkills, skillsTruncated, translateConversationError(err)
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
		audience, err := conversationReadAudienceRecord(ctx, reader, scope.organization, scope.credential)
		if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, operatortool.ErrAccessDenied) {
			audience, err = "", nil
		}
		if err != nil {
			return err
		}
		page, err = reader.Conversations(ctx, conversationstore.ConversationQuery{
			ProjectID: filter.Project, ProjectIDs: filter.Projects, PrincipalID: filter.Principal, GrantCredentialID: grantCredential, ReadAudience: audience,
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
			if record.ReadAudience != "" && record.ReadAudience != audience {
				continue
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

func coordinatorConnectionData(ctx context.Context) json.RawMessage {
	connection := operatortool.CurrentConnection(ctx)
	if connection.Client != "Luna" {
		return nil
	}
	raw, err := json.Marshal(map[string]string{"operator_connection": connection.ID, "operator_principal": connection.Identity.PrincipalID})
	if err != nil {
		return nil
	}
	return raw
}
