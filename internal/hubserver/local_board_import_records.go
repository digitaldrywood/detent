package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type localBoardImportPlan struct {
	result       localboard.Result
	project      projectstore.Record
	current      corestore.WorkItem
	issue        tracker.NativeIssue
	dependencies []corestore.WorkItem
	mutations    []localBoardImportMutation
	digest       string
}

type localBoardImportMutation struct {
	plan       hubrecords.WorkItemRecordPlan
	data       tracker.CollaborationData
	fields     hubrecords.WorkItemRecordFields
	version    eventstore.CollaborationVersion
	event      eventstore.Event
	search     searchstore.Document
	bytes      int64
	identities []postgres.DoctorProposalIdentity
}

func localBoardMapping(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, key string) (postgres.LocalImportMapping, error) {
	reader, ok := r.(interface {
		LocalImportMapping(context.Context, postgres.LocalImportMappingInput) (postgres.LocalImportMapping, error)
	})
	if !ok {
		return postgres.LocalImportMapping{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	return reader.LocalImportMapping(ctx, postgres.LocalImportMappingInput{OrganizationID: scope.Organization, ProjectID: scope.Project, SourceKey: key})
}

func (s *Service) planLocalBoardRecord(ctx context.Context, scope hubscope.NativeScope, request localboard.Request) (localBoardImportPlan, error) {
	record := request.Record
	plan := localBoardImportPlan{result: localboard.Result{SourceID: record.SourceID, SourceState: record.State, Action: "created", Comments: len(record.Comments), Dependencies: len(record.Dependencies)}}
	raw, err := json.Marshal(record)
	if err != nil {
		return plan, err
	}
	digest := sha256.Sum256(raw)
	plan.digest = hex.EncodeToString(digest[:])
	var version eventstore.CollaborationVersion
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
			return err
		}
		var err error
		plan.project, err = r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if plan.project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if plan.project.Core.Profile != "native" {
			return hubapi.NativePublicInvalid("Local board import requires a native project")
		}
		plan.result.State, plan.result.UnknownLane, err = localBoardState(plan.project.States, record.State)
		if err != nil {
			return err
		}
		mapping, err := localBoardMapping(ctx, r, scope, record.Key)
		if err == nil {
			if mapping.SourceKind != record.Kind || mapping.SourceDigest != plan.digest {
				return hubapi.NativePublicInvalid("Local board source changed after import")
			}
			plan.current, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(mapping.WorkItemID))
			if err != nil {
				return err
			}
			plan.result.Action, plan.result.WorkItemID, plan.result.State, plan.result.Comments, plan.result.Dependencies = "unchanged", tracker.NativeWorkItemID(mapping.WorkItemID), hubapi.StringValue(plan.current.WorkflowStateName), 0, 0
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if record.Kind == "github_local" {
			if err := validateLinkedIssueRepository(projectIntegrationRecord(plan.project), record.Repository); err != nil {
				return err
			}
			url := "https://github.com/" + strings.ToLower(record.Repository) + "/issues/" + strconv.Itoa(record.Number)
			ids := []tracker.NativeWorkItemID{}
			source, err := r.LinkedSourceByURL(ctx, scope.Project, url)
			if err == nil {
				ids = append(ids, source.WorkItemID)
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			item, err := r.WorkItemBySource(ctx, scope.Project, "github:"+url)
			if err == nil {
				ids = append(ids, item.NativeID)
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			slices.Sort(ids)
			ids = slices.Compact(ids)
			if len(ids) != 1 {
				return hubapi.NativePublicInvalid("GitHub local item requires exactly one existing onboarding intake match")
			}
			plan.current, err = r.WorkItem(ctx, scope.Project, ids[0])
			if err != nil {
				return err
			}
			version, err = r.CollaborationVersion(ctx, string(plan.current.NativeID), plan.current.Revision)
			if err != nil {
				return err
			}
			plan.result.Action, plan.result.WorkItemID = "updated", plan.current.NativeID
		} else {
			plan.current, err = r.WorkItemBySource(ctx, scope.Project, record.Kind+":"+record.Key)
			if err == nil {
				version, err = r.CollaborationVersion(ctx, string(plan.current.NativeID), plan.current.Revision)
				if err != nil {
					return err
				}
				plan.result.Action, plan.result.WorkItemID = "updated", plan.current.NativeID
			} else if errors.Is(err, corestore.ErrNotFound) {
				number, err := r.NextWorkItemNumber(ctx, scope.Project)
				if err != nil {
					return err
				}
				plan.issue.Number = int(number)
			} else {
				return err
			}
		}
		if plan.current.NativeID != "" {
			missing := make([]localboard.Comment, 0, len(record.Comments))
			for _, comment := range record.Comments {
				key := record.Kind + ":" + localboard.SourceKey(record.Key, "comment", record.Kind, comment.ID)
				_, err := r.CommentBySource(ctx, scope.Project, plan.current.NativeID, key)
				if errors.Is(err, corestore.ErrNotFound) {
					missing = append(missing, comment)
				} else if err != nil {
					return err
				}
			}
			record.Comments = missing
			plan.result.Comments = len(missing)
		}
		for _, key := range record.Dependencies {
			mapping, err := localBoardMapping(ctx, r, scope, key)
			if errors.Is(err, corestore.ErrNotFound) && request.DryRun {
				plan.dependencies = append(plan.dependencies, corestore.WorkItem{OrganizationID: scope.Organization, ProjectID: scope.Project, NativeID: tracker.NativeWorkItemID("dry_" + key)})
				continue
			}
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.NativePublicInvalid("Import prerequisite items before their dependents")
			}
			if err != nil {
				return err
			}
			item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(mapping.WorkItemID))
			if err != nil {
				return err
			}
			plan.dependencies = append(plan.dependencies, item)
		}
		return nil
	})
	if err != nil || plan.result.Action == "unchanged" {
		return plan, err
	}
	if plan.current.ID != 0 {
		plan.issue, err = s.hydrateWorkItemRecord(ctx, plan.current, version)
		if err != nil {
			return plan, err
		}
		plan.issue.Terminal = false
		for _, state := range plan.project.States {
			if state.Name == plan.issue.State {
				plan.issue.Terminal = state.Terminal
			}
		}
		if record.Kind == "local_sqlite" {
			plan.result.State = plan.issue.State
		}
		if plan.issue.Archived {
			return plan, hubapi.NativePublicInvalid("Local board import cannot change an archived native item")
		}
		if record.Kind == "github_local" && plan.issue.State != "Triage" && (!plan.issue.Terminal || plan.issue.State != plan.result.State) {
			return plan, hubapi.NativePublicInvalid("GitHub local lane restoration requires an intake item still in Triage")
		}
	}
	return s.planLocalBoardMutations(ctx, scope, record, plan)
}

func (s *Service) planLocalBoardMutations(ctx context.Context, scope hubscope.NativeScope, record localboard.Record, plan localBoardImportPlan) (localBoardImportPlan, error) {
	now := s.config.now().UTC().Truncate(time.Microsecond)
	provenance := &tracker.Provenance{Provider: record.Kind, ExternalID: record.Key, AuthorID: record.Author, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, ObservedAt: now}
	current := plan.current
	issue := plan.issue
	appendMutation := func(p hubrecords.WorkItemRecordPlan, data tracker.CollaborationData) {
		plan.mutations = append(plan.mutations, localBoardImportMutation{plan: p, data: data})
		if p.Comment.ID == "" {
			issue = p.Issue
			current.NativeID = issue.WorkItemID
			current.Revision = int64(issue.Revision)
			current.WorkflowStateName = new(issue.State)
		}
		current.EventSequence++
	}
	if record.Kind == "local_sqlite" && plan.current.NativeID == "" {
		request := tracker.CreateIssue{Title: record.Title, Body: record.Body, Priority: record.Priority, State: plan.result.State, Provenance: provenance}
		if err := validateWorkItemRecordCreateInput(ctx, scope, plan.project, request); err != nil {
			return plan, err
		}
		p, err := s.planWorkItemRecord(ctx, scope, hubrecords.WorkItemRecordPlan{Project: plan.project, Issue: issue}, request)
		if err != nil {
			return plan, err
		}
		appendMutation(p, tracker.CollaborationData{Revision: p.Issue.Revision})
	} else if record.Kind == "github_local" && issue.State != plan.result.State {
		p := hubrecords.WorkItemRecordPlan{Project: plan.project, Current: current, Issue: issue, EventType: "workflow.transitioned"}
		p.Issue.State = plan.result.State
		p.Issue.Revision++
		p.Issue.UpdatedAt, p.Issue.LastActivityAt = now, now
		for _, state := range plan.project.States {
			if state.Name == p.Issue.State {
				p.Issue.Terminal = state.Terminal
			}
		}
		p.Issue.ClosedAt = nil
		if p.Issue.Terminal {
			p.Issue.ClosedAt = &now
		}
		appendMutation(p, tracker.CollaborationData{Revision: p.Issue.Revision, FromState: issue.State, ToState: p.Issue.State, Reason: "user_requested"})
	}
	for _, comment := range record.Comments {
		source := *provenance
		source.ExternalID = localboard.SourceKey(record.Key, "comment", record.Kind, comment.ID)
		source.AuthorID = "detent"
		source.CreatedAt, source.UpdatedAt = comment.CreatedAt, comment.UpdatedAt
		p, err := s.planWorkItemRecord(ctx, scope, hubrecords.WorkItemRecordPlan{Project: plan.project, Current: current, Issue: issue}, tracker.CreateComment{Body: comment.Body, Provenance: &source})
		if err != nil {
			return plan, err
		}
		appendMutation(p, tracker.CollaborationData{Revision: p.Comment.Revision, CommentID: p.Comment.ID})
	}
	plan.result.Dependencies = 0
	for _, related := range plan.dependencies {
		if slices.Contains(issue.Dependencies, related.NativeID) {
			continue
		}
		issue.Dependencies = slices.Clone(issue.Dependencies)
		relationship := &corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: current.NativeID, RelatedProjectID: scope.Project, RelatedWorkItemID: related.NativeID, Type: "dependency"}
		p := hubrecords.WorkItemRecordPlan{Project: plan.project, Current: current, Issue: issue, Related: related, Relationship: relationship, RelationshipType: "dependency", RelationshipOperate: "add"}
		p, err := s.planWorkItemRecord(ctx, scope, p, tracker.DependencyMutation{ExpectedRevision: issue.Revision, RelatedWorkItemID: related.NativeID, Operation: "add"})
		if err != nil {
			return plan, err
		}
		appendMutation(p, tracker.CollaborationData{Revision: p.Issue.Revision, RelatedWorkItemID: related.NativeID, Operation: "add"})
		plan.result.Dependencies++
	}
	plan.result.WorkItemID = issue.WorkItemID
	plan.issue = issue
	return plan, nil
}

func (s *Service) prepareLocalBoardMutation(ctx context.Context, scope hubscope.NativeScope, m localBoardImportMutation, dry bool) (localBoardImportMutation, error) {
	p := m.plan
	id, item, revision, at := string(p.Issue.WorkItemID), p.Issue.WorkItemID, int64(p.Issue.Revision), p.Issue.UpdatedAt
	text := searchstore.Text{Title: p.Issue.Title, Body: p.Issue.Body, Labels: p.Issue.Labels}
	kind := "issue"
	if p.Comment.ID != "" {
		id, item, revision, at = p.Comment.ID, p.Comment.WorkItemID, int64(p.Comment.Revision), p.Comment.UpdatedAt
		kind = "comment"
		text = searchstore.Text{Body: p.Comment.Body}
	}
	raw, err := json.Marshal(p.Value())
	if err != nil {
		return m, err
	}
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, ID: hubapi.NewID("evt"), Type: p.EventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: at, Data: m.data})
	if err != nil {
		return m, err
	}
	dataRaw, err := json.Marshal(m.data)
	if err != nil {
		return m, err
	}
	actorRaw, err := json.Marshal(scope.Actor())
	if err != nil {
		return m, err
	}
	m.bytes = int64(len(raw) + len(dataRaw) + len(actorRaw))
	if p.Comment.ID != "" {
		m.bytes += int64(len(p.Comment.Body))
	} else if p.Create {
		labels, err := json.Marshal(p.Issue.Labels)
		if err != nil {
			return m, err
		}
		assignees, err := json.Marshal(p.Issue.Assignees)
		if err != nil {
			return m, err
		}
		m.bytes += int64(len(p.Issue.Title) + len(p.Issue.Body) + len(labels) + len(assignees))
	}
	if dry {
		return m, nil
	}
	if p.Comment.ID == "" {
		m.identities = postgres.DeriveDoctorProposalIdentities(p.Issue.Body)
	}
	m.fields, err = s.prepareWorkItemFields(ctx, scope.Organization, p)
	if err != nil {
		return m, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	m.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, RecordID: id, Revision: revision}, raw)
	if err != nil {
		return m, err
	}
	m.event, err = content.PrepareEvent(ctx, scope.Organization, eventRaw)
	if err != nil {
		return m, err
	}
	m.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, ID: kind + ":" + id, SourceID: id, Kind: kind, Scope: "issue", Revision: revision, UpdatedAt: at}, text)
	return m, err
}

func (s *Service) importLocalBoardRecord(ctx context.Context, scope hubscope.NativeScope, request localboard.Request) (localboard.Result, error) {
	scope.ImportedContent = true
	if s.config.TenantRepository == nil {
		return localboard.Result{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	for {
		if err := ctx.Err(); err != nil {
			return localboard.Result{}, err
		}
		if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
			return localboard.Result{}, err
		}
		plan, err := s.planLocalBoardRecord(ctx, scope, request)
		if err != nil || plan.result.Action == "unchanged" {
			return plan.result, hubapi.ProjectRecordError(err)
		}
		for i, m := range plan.mutations {
			plan.mutations[i], err = s.prepareLocalBoardMutation(ctx, scope, m, request.DryRun)
			if err != nil {
				return plan.result, err
			}
		}
		if request.DryRun {
			err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { return s.localBoardAllowances(ctx, scope, r, nil, plan) })
			return plan.result, hubapi.ProjectRecordError(err)
		}
		repository, ok := s.config.TenantRepository.(interface {
			WriteLocalBoardImport(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error
		})
		if !ok {
			return plan.result, hubapi.NativeWorkItemStorageUnavailable()
		}
		authority, err := s.beginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return plan.result, err
		}
		err = repository.WriteLocalBoardImport(ctx, scope.Organization, func(w tenantstore.Writer) error {
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			mapping, err := localBoardMapping(ctx, w, scope, request.Record.Key)
			if err == nil {
				if mapping.SourceDigest != plan.digest || mapping.SourceKind != request.Record.Kind {
					return hubapi.NativePublicInvalid("Local board source changed after import")
				}
				return corestore.ErrSerializationFailure
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if len(plan.dependencies) > 0 {
				if err := w.LockRelationshipGraph(ctx); err != nil {
					return err
				}
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			project, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			if !hubrecords.SameWorkItemProjectPlan(plan.project, project) || project.Settings.CheckoutRepository != plan.project.Settings.CheckoutRepository {
				return corestore.ErrSerializationFailure
			}
			endpoints := slices.Clone(plan.dependencies)
			if plan.current.NativeID != "" {
				endpoints = append(endpoints, plan.current)
			}
			slices.SortFunc(endpoints, func(a, b corestore.WorkItem) int { return strings.Compare(string(a.NativeID), string(b.NativeID)) })
			for _, endpoint := range slices.CompactFunc(endpoints, func(a, b corestore.WorkItem) bool { return a.NativeID == b.NativeID }) {
				current, err := w.LockWorkItem(ctx, scope.Project, endpoint.NativeID)
				if err != nil {
					return err
				}
				if current.Revision != endpoint.Revision || current.EventSequence != endpoint.EventSequence {
					return corestore.ErrSerializationFailure
				}
			}
			if err := s.localBoardAllowances(ctx, scope, w, w, plan); err != nil {
				return err
			}
			for _, m := range plan.mutations {
				p := m.plan
				if p.Current.NativeID != "" {
					current, err := w.LockWorkItem(ctx, scope.Project, p.Current.NativeID)
					if err != nil {
						return err
					}
					if current.Revision != p.Current.Revision || current.EventSequence != p.Current.EventSequence {
						return corestore.ErrSerializationFailure
					}
					p.Current = current
				}
				if p.Relationship != nil {
					if err := w.SetRelationship(ctx, *p.Relationship, "add", p.Issue.UpdatedAt); err != nil {
						return err
					}
				}
				if err := hubrecords.WriteWorkItemRecord(ctx, w, p, m.fields); err != nil {
					return err
				}
				previous := p.Current.Revision
				if p.Comment.ID != "" {
					previous = 0
				}
				if err := w.PutSearch(ctx, m.search, previous); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(ctx, m.version); err != nil {
					return err
				}
				if p.Comment.ID == "" {
					binder, ok := w.(interface {
						BindDoctorProposalIdentities(context.Context, postgres.DoctorProposalIdentityBinding) error
					})
					if !ok {
						return hubapi.NativeWorkItemStorageUnavailable()
					}
					if err := binder.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: m.version.WorkItemID, Revision: m.version.Revision, VersionSHA256: m.version.PayloadSHA256, Identities: m.identities}); err != nil {
						return err
					}
				}
				if _, err := w.AppendEvent(ctx, m.event); err != nil {
					return err
				}
			}
			writer, ok := w.(interface {
				PutLocalImportMapping(context.Context, postgres.PutLocalImportMappingInput) (postgres.LocalImportMapping, error)
			})
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			if _, err := writer.PutLocalImportMapping(ctx, postgres.PutLocalImportMappingInput{OrganizationID: scope.Organization, ProjectID: scope.Project, SourceKey: request.Record.Key, SourceKind: request.Record.Kind, SourceDigest: plan.digest, WorkItemID: string(plan.result.WorkItemID), ImportedAt: s.config.now()}); err != nil {
				return err
			}
			return s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), 200)
		})
		err = errors.Join(err, hubrecords.ReleaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err == nil {
			select {
			case s.boardRefresh <- struct{}{}:
			default:
			}
		}
		return plan.result, hubapi.ProjectRecordError(err)
	}
}

func (s *Service) localBoardAllowances(ctx context.Context, scope hubscope.NativeScope, r tenantstore.Reader, w tenantstore.Writer, plan localBoardImportPlan) error {
	if s.database.plansService().Plans() == nil {
		return nil
	}
	now := s.config.now()
	if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, r, "collaboration", now)); err != nil {
		return err
	}
	entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, r, now))
	if err != nil {
		return err
	}
	var bytes int64
	for _, m := range plan.mutations {
		bytes += m.bytes
	}
	usage, err := r.CollaborationBytes(ctx)
	if err != nil {
		return err
	}
	if limit, limited := entitlement.Allowances["collaboration_bytes"]; limited && usage+bytes > limit {
		return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage}
	}
	if w != nil {
		limit := int64(math.MaxInt64)
		if value, limited := entitlement.Allowances["collaboration_bytes"]; limited {
			limit = value
		}
		result, err := w.AdjustCollaborationBytes(ctx, bytes, limit)
		if err != nil {
			return err
		}
		if !result.Allowed {
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: result.Used}
		}
	}
	plans := s.database.plansService().Plans()
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	for _, value := range []struct {
		metric string
		amount int64
	}{{"api_mutations", 1}, {"ingested_events", int64(len(plan.mutations))}} {
		if value.amount == 0 || value.metric == "api_mutations" && ctx.Value(runnerRequestBudgetContext{}) == true {
			continue
		}
		used, err := r.BillingUsage(ctx, window, value.metric)
		if errors.Is(err, corestore.ErrNotFound) {
			used, err = 0, nil
		}
		if err != nil {
			return err
		}
		limit := entitlement.Allowances[value.metric]
		if used+value.amount > limit {
			return &hostedplans.LimitError{Resource: value.metric, Allowance: limit, Consumption: used}
		}
		if w != nil {
			result, err := w.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: scope.Organization, ID: "local_import/" + string(scope.Project) + "/" + plan.digest + "/" + value.metric, WindowStart: window, Metric: value.metric, Amount: value.amount}, limit)
			if err != nil {
				return err
			}
			if !result.Allowed {
				return &hostedplans.LimitError{Resource: value.metric, Allowance: limit, Consumption: result.Used}
			}
		}
	}
	reader, ok := r.(interface {
		LocalBoardImportAllowanceUsage(context.Context) (int64, int64, error)
	})
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	issues, history, err := reader.LocalBoardImportAllowanceUsage(ctx)
	if errors.Is(err, corestore.ErrNotFound) {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	if err != nil {
		return err
	}
	growth := int64(0)
	if !plan.issue.Archived && !plan.issue.Terminal {
		growth++
	}
	if plan.current.NativeID != "" && !plan.current.Archived {
		terminal := false
		for _, state := range plan.project.States {
			if state.Name == hubapi.StringValue(plan.current.WorkflowStateName) {
				terminal = state.Terminal
			}
		}
		if !terminal {
			growth--
		}
	}
	for _, value := range []struct {
		metric      string
		used, delta int64
	}{{"unarchived_issues", issues, growth}, {"history_records", history, 2 * int64(len(plan.mutations))}} {
		if limit, limited := entitlement.Allowances[value.metric]; limited && value.delta > 0 && value.used+value.delta > limit {
			return &hostedplans.LimitError{Resource: value.metric, Allowance: limit, Consumption: value.used}
		}
	}
	return nil
}
