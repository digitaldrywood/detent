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
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const coordinatorSplitMaxChildren = 10

func coordinatorIssueSplitTool() runner.AgentTool {
	return coordinatorTool(string(chat.ActionIssueSplit), "Propose an entire issue split for one client submission. The client controls inline confirmation; no issues are created by the proposal. Children use positions 1 through 10; position 0 is the parent. Each edge means dependent is blocked by blocker. Priority is 0 urgent through 3 low, omitted for unset.", `{"type":"object","required":["parent_work_item_id","children","edges"],"properties":{"parent_work_item_id":{"type":"string"},"children":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"object","required":["title","description","state"],"properties":{"title":{"type":"string","maxLength":500},"description":{"type":"string","maxLength":4000},"priority":{"type":"integer","minimum":0,"maximum":3},"state":{"type":"string"}},"additionalProperties":false}},"edges":{"type":"array","maxItems":100,"items":{"type":"object","required":["dependent","blocker"],"properties":{"dependent":{"type":"integer","minimum":0,"maximum":10},"blocker":{"type":"integer","minimum":0,"maximum":10}},"additionalProperties":false}}},"additionalProperties":false}`)
}

func (t *coordinatorToolset) loadSplitIssueSkill(raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decodeCoordinatorArguments(raw, &args); err != nil {
		return nil, err
	}
	skill, body, err := skills.ReadBuiltin("split-issue")
	if err != nil {
		return nil, err
	}
	if len(body) > hubapi.CoordinatorToolArgumentBytes {
		return nil, fmt.Errorf("split-issue skill exceeds %d bytes", hubapi.CoordinatorToolArgumentBytes)
	}
	return map[string]string{"name": skill.Name, "instructions": body}, nil
}

func validateCoordinatorIssueSplit(split chat.IssueSplit) error {
	if !strings.HasPrefix(split.ParentID, "wi_") || len(split.ParentID) > 256 {
		return hubapi.NativePublicInvalid("A native parent work item is required")
	}
	if len(split.Children) < 1 || len(split.Children) > coordinatorSplitMaxChildren {
		return hubapi.NativePublicInvalid("A split must contain 1 to 10 children")
	}
	for _, child := range split.Children {
		if strings.TrimSpace(child.Title) == "" || len(child.Title) > coordinatorProposalTitleBytes || strings.TrimSpace(child.Description) == "" || utf8.RuneCountInString(child.Description) > coordinatorProposalBodyRunes || strings.TrimSpace(child.State) == "" || len(child.State) > 256 {
			return hubapi.NativePublicInvalid("Each child requires a bounded title, description and target state")
		}
		if child.Priority != nil && (*child.Priority < 0 || *child.Priority > 3) {
			return hubapi.NativePublicInvalid("Child priority must be 0 through 3")
		}
	}
	if len(split.Edges) > 100 {
		return hubapi.NativePublicInvalid("A split may contain at most 100 dependency edges")
	}
	graph := make([][]int, len(split.Children)+1)
	seen := map[chat.IssueSplitEdge]bool{}
	for _, edge := range split.Edges {
		if edge.Dependent < 0 || edge.Dependent > len(split.Children) || edge.Blocker < 0 || edge.Blocker > len(split.Children) {
			return hubapi.NativePublicInvalid("Dependency edge references an unknown child")
		}
		if seen[edge] {
			return hubapi.NativePublicInvalid("Dependency edges must be unique")
		}
		seen[edge] = true
		graph[edge.Dependent] = append(graph[edge.Dependent], edge.Blocker)
	}
	visited := make([]int, len(graph))
	var visit func(int) bool
	visit = func(node int) bool {
		if visited[node] == 1 {
			return false
		}
		if visited[node] == 2 {
			return true
		}
		visited[node] = 1
		for _, blocker := range graph[node] {
			if !visit(blocker) {
				return false
			}
		}
		visited[node] = 2
		return true
	}
	for node := range graph {
		if !visit(node) {
			return hubapi.NativePublicInvalid("Dependencies cannot form a cycle")
		}
	}
	return nil
}

func (t *coordinatorToolset) proposeIssueSplit(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	var split chat.IssueSplit
	if err := decodeCoordinatorArguments(call.Arguments, &split); err != nil {
		return nil, err
	}
	if split.Revision != 0 {
		return nil, operatortool.ErrInvalidArguments
	}
	if err := validateCoordinatorIssueSplit(split); err != nil {
		return nil, err
	}
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(record.ProjectID), ResourceKind: "work_item", ResourceID: split.ParentID})
	if err != nil {
		return nil, err
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	scope.Project = record.ProjectID
	s := t.coordinator.server
	row, version, err := s.readWorkItemRecord(readCtx, scope, split.ParentID)
	if err != nil {
		return nil, err
	}
	parent, err := s.hydrateWorkItemRecord(readCtx, row, version)
	if err != nil {
		return nil, err
	}
	split.Revision = int64(parent.Revision)
	if err := hubrecords.RequireNativeEdit(parent, parent.Revision); err != nil {
		return nil, err
	}
	project, err := s.readProjectRecord(readCtx, scope)
	if err != nil {
		return nil, err
	}
	for _, child := range split.Children {
		if err := validateWorkItemRecordCreateInput(readCtx, scope, project, tracker.CreateIssue{Title: child.Title, Body: child.Description, Priority: child.Priority}); err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool { return state.Name == child.State }) {
			return nil, hubapi.NativePublicInvalid("Workflow state does not exist")
		}
	}
	arguments, err := json.Marshal(split)
	if err != nil {
		return nil, err
	}
	if len(arguments) > hubapi.CoordinatorToolArgumentBytes {
		return nil, fmt.Errorf("%w: approved split exceeds %d bytes", errCoordinatorToolArguments, hubapi.CoordinatorToolArgumentBytes)
	}
	action := chat.Action{RequiresConfirmation: true, ConversationID: record.ID, Kind: chat.ActionIssueSplit, ProjectID: string(record.ProjectID), IssueID: split.ParentID, Identifier: fmt.Sprintf("#%d", parent.Number), Revision: split.Revision, Title: fmt.Sprintf("Split #%d: %s into %d issues", parent.Number, parent.Title, len(split.Children)), Arguments: arguments, Material: true}
	return t.submitCoordinatorAction(ctx, record, call, action)
}

type coordinatorSplitResult struct {
	Children []tracker.NativeIssue `json:"children"`
	Comment  tracker.NativeComment `json:"comment"`
}

func (s *Service) executeCoordinatorIssueSplit(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	var split chat.IssueSplit
	if err := decodeCoordinatorArguments(action.Arguments, &split); err != nil {
		return chat.ActionExecution{}, err
	}
	if split.ParentID != action.IssueID || split.Revision != action.Revision {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	resolve, ok := ctx.Value(hubscope.NativeOperatorScopeKey{}).(func(context.Context) (hubscope.NativeScope, error))
	if !ok {
		return chat.ActionExecution{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return chat.ActionExecution{}, err
	}
	scope.Project = tracker.ProjectID(action.ProjectID)
	options := hubapi.NativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+split.ParentID+"/split"), Item: split.ParentID, RequireLease: true, Feature: "collaboration"}
	raw, err := s.executeCoordinatorSplitRecords(ctx, scope, options, tracker.MutationForContext(ctx, action.RequestID), split)
	if err != nil {
		return chat.ActionExecution{}, coordinatorActionError(err)
	}
	var result coordinatorSplitResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return chat.ActionExecution{}, err
	}
	for _, child := range result.Children {
		encoded, err := json.Marshal(child)
		if err != nil {
			return chat.ActionExecution{}, err
		}
		s.wakeSpriteRunnersAfter(scope, encoded)
	}
	var names []string
	for _, child := range result.Children {
		names = append(names, fmt.Sprintf("#%d (%s)", child.Number, child.WorkItemID))
	}
	return chat.ActionExecution{Message: "Created " + strings.Join(names, ", ") + " with their dependencies and parent comment.", ResourceID: split.ParentID, Identifier: action.Identifier, Revision: split.Revision, Data: raw}, nil
}

type coordinatorSplitRecord struct {
	plan         hubrecords.WorkItemRecordPlan
	fields       hubrecords.WorkItemRecordFields
	version      eventstore.CollaborationVersion
	event        eventstore.Event
	search       searchstore.Document
	attachments  []string
	policy       policystore.Revision
	policyFound  bool
	commentBytes int64
}

func (s *Service) prepareCoordinatorSplitRecord(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan) (coordinatorSplitRecord, error) {
	record := coordinatorSplitRecord{plan: plan}
	var err error
	if plan.RequiresIssueContract() {
		record.policy, record.policyFound, err = s.validateWorkItemRecordFilingTenant(ctx, scope, plan)
		if err != nil {
			return record, err
		}
	}
	if plan.Create {
		attachments, err := s.prepareWorkItemAttachmentReferences(ctx, scope, plan)
		if err != nil {
			return record, err
		}
		record.attachments = attachments.Current
	}
	record.fields, err = s.prepareWorkItemFields(ctx, scope.Organization, plan)
	if err != nil {
		return record, err
	}
	raw, err := json.Marshal(plan.Value())
	if err != nil {
		return record, err
	}
	id, revision, at := string(plan.Issue.WorkItemID), int64(plan.Issue.Revision), plan.Issue.UpdatedAt
	item := plan.Issue.WorkItemID
	text := searchstore.Text{Title: plan.Issue.Title, Body: plan.Issue.Body, Labels: plan.Issue.Labels}
	kind := "issue"
	data := tracker.CollaborationData{Revision: plan.Issue.Revision, Fields: plan.Fields}
	if plan.Relationship != nil {
		data.RelatedWorkItemID, data.Operation = plan.Related.NativeID, plan.RelationshipOperate
	}
	if plan.Comment.ID != "" {
		id, revision, item, at = plan.Comment.ID, int64(plan.Comment.Revision), plan.Comment.WorkItemID, plan.Comment.UpdatedAt
		kind, text = "comment", searchstore.Text{Body: plan.Comment.Body}
		data.Revision, data.CommentID = plan.Comment.Revision, plan.Comment.ID
	}
	if plan.Comment.ID != "" && len(record.attachments) > 0 {
		dataRaw, err := json.Marshal(data)
		if err != nil {
			return record, err
		}
		actorRaw, err := json.Marshal(scope.Actor())
		if err != nil {
			return record, err
		}
		record.commentBytes = int64(len(plan.Comment.Body) + len(raw) + len(dataRaw) + len(actorRaw))
	}
	content := eventstore.NewContent(s.runDetailStore())
	record.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, RecordID: id, Revision: revision}, raw)
	if err != nil {
		return record, err
	}
	record.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, ID: kind + ":" + id, SourceID: id, Kind: kind, Scope: "issue", Revision: revision, UpdatedAt: at}, text)
	if err != nil {
		return record, err
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, ID: hubapi.NewID("evt"), Type: plan.EventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: at, Data: data})
	if err != nil {
		return record, err
	}
	record.event, err = content.PrepareEvent(ctx, scope.Organization, eventRaw)
	return record, err
}

func (s *Service) readCoordinatorSplitReceipt(ctx context.Context, scope hubscope.NativeScope, receipt eventstore.Receipt, parent string) (json.RawMessage, error) {
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	if err != nil {
		return nil, err
	}
	var result coordinatorSplitResult
	if json.Unmarshal(raw, &result) != nil || len(result.Children) < 1 || len(result.Children) > coordinatorSplitMaxChildren || result.Comment.OrganizationID != scope.Organization || result.Comment.ProjectID != scope.Project || result.Comment.WorkItemID != tracker.NativeWorkItemID(parent) {
		return nil, receiptstore.ErrExpired
	}
	for _, child := range result.Children {
		if child.OrganizationID != scope.Organization || child.ProjectID != scope.Project || child.WorkItemID == "" {
			return nil, receiptstore.ErrExpired
		}
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	if _, _, err := s.readWorkItemRecord(ctx, scope, parent); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *Service) executeCoordinatorSplitRecords(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, split chat.IssueSplit) (json.RawMessage, error) {
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := validateCoordinatorIssueSplit(split); err != nil {
		return nil, err
	}
	if split.Revision <= 0 || strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("A current revision and idempotency key are required")
	}
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		return nil, operatortool.ErrAccessDenied
	}
	scope.Action, scope.ActionItem = string(chat.ActionIssueSplit), split.ParentID
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(split)
	if err != nil {
		return nil, err
	}
	receipt, found, err := s.workItemRecordReceipt(ctx, scope, options.OperationID, command.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return s.readCoordinatorSplitReceipt(ctx, scope, receipt, split.ParentID)
	}
	parent, err := s.loadWorkItemPlan(ctx, scope, split.ParentID, "", tracker.CreateComment{})
	if err != nil {
		return nil, err
	}
	if err := hubrecords.RequireNativeEdit(parent.Issue, tracker.Revision(split.Revision)); err != nil {
		return nil, err
	}
	var nextNumber int64
	var organizationSlug string
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		nextNumber, err = r.NextWorkItemNumber(ctx, scope.Project)
		if err != nil {
			return err
		}
		organization, err := r.AccessOrganization(ctx)
		organizationSlug = organization.Slug
		return err
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	nodes := []tracker.NativeIssue{parent.Issue}
	rows := []corestore.WorkItem{parent.Current}
	records := make([]coordinatorSplitRecord, 0, len(split.Children)+len(split.Edges)+1)
	for index, child := range split.Children {
		request := tracker.CreateIssue{Title: child.Title, Body: child.Description, Priority: child.Priority, State: child.State}
		if err := validateWorkItemRecordCreateInput(ctx, scope, parent.Project, request); err != nil {
			return nil, err
		}
		plan := hubrecords.WorkItemRecordPlan{Project: parent.Project, Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{Number: int(nextNumber) + index}}}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, request)
		if err != nil {
			return nil, err
		}
		plan.Issue.WebURL = s.nativeIssueResponse(tracker.NativeIssue{WebURL: hubrecords.IssueBrowserPath(organizationSlug, parent.Project.Core.Slug, plan.Issue.Number)}).WebURL
		record, err := s.prepareCoordinatorSplitRecord(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		nodes = append(nodes, plan.Issue)
		rows = append(rows, corestore.WorkItem{OrganizationID: scope.Organization, ProjectID: scope.Project, NativeID: plan.Issue.WorkItemID, Revision: int64(plan.Issue.Revision), EventSequence: 1, WorkflowStateName: &plan.Issue.State})
	}
	for _, edge := range split.Edges {
		dependent, blocker := nodes[edge.Dependent], nodes[edge.Blocker]
		plan := hubrecords.WorkItemRecordPlan{Project: parent.Project, Current: rows[edge.Dependent], Related: rows[edge.Blocker], Issue: dependent,
			PreviousContent: dependent.Title + "\n" + dependent.Body, RelationshipType: "dependency", RelationshipOperate: "add",
			Relationship: &corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: dependent.WorkItemID, RelatedProjectID: scope.Project, RelatedWorkItemID: blocker.WorkItemID, Type: "dependency"}}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, tracker.DependencyMutation{ExpectedRevision: dependent.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"})
		if err != nil {
			return nil, err
		}
		record, err := s.prepareCoordinatorSplitRecord(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		nodes[edge.Dependent] = plan.Issue
		rows[edge.Dependent].Revision = int64(plan.Issue.Revision)
		rows[edge.Dependent].EventSequence++
	}
	var body strings.Builder
	body.WriteString("Approved issue split:\n")
	for _, child := range nodes[1:] {
		fmt.Fprintf(&body, "\n- #%d (%s): %s", child.Number, child.WorkItemID, child.Title)
	}
	if len(split.Edges) > 0 {
		body.WriteString("\n\nDependencies:\n")
		for _, edge := range split.Edges {
			fmt.Fprintf(&body, "\n- #%d is blocked by #%d", nodes[edge.Dependent].Number, nodes[edge.Blocker].Number)
		}
	}
	commentPlan := hubrecords.WorkItemRecordPlan{Project: parent.Project, Current: rows[0], Issue: nodes[0]}
	commentPlan, err = s.planWorkItemRecord(ctx, scope, commentPlan, tracker.CreateComment{Body: body.String()})
	if err != nil {
		return nil, err
	}
	commentRecord, err := s.prepareCoordinatorSplitRecord(ctx, scope, commentPlan)
	if err != nil {
		return nil, err
	}
	records = append(records, commentRecord)
	raw, err := json.Marshal(coordinatorSplitResult{Children: nodes[1:], Comment: commentPlan.Comment})
	if err != nil {
		return nil, err
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	receipt, err = eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: options.OperationID, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	authority, err := s.beginProjectRecordAuthority(ctx, scope)
	if err != nil {
		return nil, err
	}
	var previous *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if len(split.Edges) > 0 {
			if err := w.LockRelationshipGraph(ctx); err != nil {
				return err
			}
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" {
			return corestore.ErrNotFound
		}
		if !hubrecords.SameWorkItemProjectPlan(parent.Project, project) {
			return hubapi.NativeConflict(tracker.Revision(parent.Current.Revision))
		}
		stored, err := w.Receipt(ctx, scope.Credential.ID, options.OperationID, command.IdempotencyKey)
		if err == nil {
			if stored.ResponseExpired || stored.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= stored.ResponseReplayUntilNs {
				return receiptstore.ErrExpired
			}
			if stored.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			previous = &stored
			return s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK)
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		created := map[tracker.NativeWorkItemID]bool{}
		for _, record := range records {
			if record.plan.Create {
				created[record.plan.Issue.WorkItemID] = true
			}
		}
		refs := []corestore.WorkItemReference{{ProjectID: scope.Project, WorkItemID: parent.Current.NativeID}}
		for _, record := range records {
			if (!record.plan.Create || record.plan.Comment.ID != "") && !created[record.plan.Current.NativeID] {
				refs = append(refs, corestore.WorkItemReference{ProjectID: scope.Project, WorkItemID: record.plan.Current.NativeID})
			}
		}
		if _, err := w.LockWorkItems(ctx, refs); err != nil {
			return err
		}
		current, err := w.LockWorkItem(ctx, scope.Project, parent.Current.NativeID)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		if current.Revision != parent.Current.Revision || current.EventSequence != parent.Current.EventSequence || current.Archived {
			return hubapi.NativeConflict(tracker.Revision(current.Revision))
		}
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, options.Feature, now)); err != nil {
			return err
		}
		policy, policyErr := w.ProjectPolicy(ctx, scope.Project)
		if policyErr != nil && !errors.Is(policyErr, corestore.ErrNotFound) {
			return policyErr
		}
		for _, record := range records {
			plan := record.plan
			if plan.RequiresIssueContract() && (record.policyFound != (policyErr == nil) || record.policyFound && !hubrecords.SameNativeClaimProjectPolicy(record.policy, policy)) {
				return hubapi.PolicyMismatch("The approved policy changed after the split proposal")
			}
			if !plan.Create || plan.Comment.ID != "" {
				current, err := w.LockWorkItem(ctx, scope.Project, plan.Current.NativeID)
				if err != nil {
					return err
				}
				if current.Revision != plan.Current.Revision || current.EventSequence != plan.Current.EventSequence {
					return hubapi.NativeConflict(tracker.Revision(current.Revision))
				}
				plan.Current = current
			}
			if plan.Relationship != nil {
				if err := w.SetRelationship(ctx, *plan.Relationship, plan.RelationshipOperate, plan.Issue.UpdatedAt); err != nil {
					return err
				}
			}
			if record.commentBytes > 0 {
				limit, err := s.attachmentQuotaLimit(ctx, w)
				if err != nil {
					return err
				}
				usage, err := w.AdjustCollaborationBytes(ctx, record.commentBytes, limit)
				if errors.Is(err, corestore.ErrNotFound) {
					return hubapi.AttachmentAccountingUnavailable()
				}
				if err != nil {
					return err
				}
				if !usage.Allowed {
					return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
				}
			}
			if err := hubrecords.WriteWorkItemRecord(ctx, w, plan, record.fields); err != nil {
				return err
			}
			searchRevision := plan.Current.Revision
			if plan.Comment.ID != "" {
				searchRevision = plan.CommentRow.Revision
			}
			if err := w.PutSearch(ctx, record.search, searchRevision); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(ctx, record.version); err != nil {
				return err
			}
			if _, err := w.AppendEvent(ctx, record.event); err != nil {
				return err
			}
			item := plan.Issue.WorkItemID
			if plan.Comment.ID != "" {
				item = plan.Comment.WorkItemID
			}
			for _, id := range record.attachments {
				if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.Project, AttachmentID: id, WorkItemID: item, CommentID: plan.Comment.ID}); err != nil {
					return err
				}
			}
		}
		if err := s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	err = errors.Join(err, hubrecords.ReleaseWorkItemAuthority(authority))
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if previous != nil {
		return s.readCoordinatorSplitReceipt(ctx, scope, *previous, split.ParentID)
	}
	s.signalNativeDispatch(scope)
	return raw, nil
}
