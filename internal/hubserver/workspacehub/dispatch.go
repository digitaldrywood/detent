package workspacehub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Workspace dispatch. A workspace is its own work item
// kind, detent:workspace, distinct from the runner-dispatched coordinator kind: it
// may check out the repository, it does not end when a turn ends, and it stays
// claimed until the workspace closes.
//
// The claim gate is therefore stricter than the coordinator's. A runner may
// claim a workspace item only if it reports every capability in requires with a
// fresh heartbeat, holds an active grant for the project, and -- when the
// workspace names an attempt whose worktree is still retained -- is the runner
// that ran that attempt, because nobody else has that worktree.

const (
	// ItemLabel marks the native issue as a workspace session. The
	// runner dispatcher selects its hold-open mode from this label.
	ItemLabel = "detent:workspace"
	// workspaceItemTitlePrefix names the issue after the workspace it opens.
	// The subject issue's title is deliberately not repeated: the workspace
	// item is a dispatch record, not a second copy of the work.
	workspaceItemTitlePrefix = "Workspace session "
)

// workspaceLabelContextKey marks the hub's own workspace-item creation. It is
// unexported and never derived from a request, so no API caller can reserve the
// label for itself.
type workspaceLabelContextKey struct{}

// AllowLabel authorizes the reserved workspace label for one call.
func AllowLabel(ctx context.Context) context.Context {
	return context.WithValue(ctx, workspaceLabelContextKey{}, true)
}

// LabelAllowed reports whether this call may set the reserved label.
func LabelAllowed(ctx context.Context) bool {
	allowed, isBool := ctx.Value(workspaceLabelContextKey{}).(bool)
	return isBool && allowed
}

// workspaceItemTitle names the issue after the workspace.
func workspaceItemTitle(workspaceID string) string {
	return workspaceItemTitlePrefix + workspaceID
}

// workspaceItemBody states what the runner is being asked for. The body is the
// whole brief a claiming runner sees before it binds.
func workspaceItemBody(record Record) string {
	body := "Workspace session " + record.ID + " on work item " + record.SubjectWorkItemID + ".\n\n" +
		"This item asks a runner to open a worktree and hold it for a person's surfaces. " +
		"No implementation, branch or pull request is expected: the runner binds through the workspace " +
		"worker endpoints, heartbeats, serves the relay and unbinds when the workspace closes.\n\n" +
		"Surfaces required: " + joinWords(record.Requires) + ".\nRef: " + record.Ref + "."
	if record.AttemptID != "" {
		body += "\nAttempt: " + record.AttemptID + "."
	}
	if record.HeadSHA != "" {
		body += "\nHead: " + record.HeadSHA + "."
	}
	if record.ReadOnly {
		body += "\n\nThe subject attempt is still running, so this workspace is read-only and refuses a terminal."
	}
	return body
}

// joinWords renders a list for the issue body.
func joinWords(values []string) string {
	switch len(values) {
	case 0:
		return "none"
	case 1:
		return values[0]
	}
	joined := ""
	for index, value := range values {
		switch {
		case index == 0:
			joined = value
		case index == len(values)-1:
			joined += " and " + value
		default:
			joined += ", " + value
		}
	}
	return joined
}

type IssueClosePlan struct {
	plan    hubrecords.WorkItemRecordPlan
	fields  hubrecords.WorkItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
	move    bool
}

func (o *Operations) PrepareIssueClose(ctx context.Context, scope hubscope.NativeScope, item string, now time.Time) (IssueClosePlan, error) {
	var out IssueClosePlan
	if item == "" {
		return out, nil
	}
	plan, err := o.LoadWorkItemPlan(ctx, scope, item, "", tracker.UpdateIssue{})
	if err != nil {
		return out, err
	}
	terminal, found := FirstTerminalState(tracker.NativeProject{States: plan.Project.States})
	if !found || plan.Issue.State == terminal {
		return out, nil
	}
	from := plan.Issue.State
	plan.Issue.State, plan.Issue.Terminal = terminal, true
	plan.Issue.Revision++
	plan.Issue.UpdatedAt, plan.Issue.LastActivityAt = now, now
	plan.Issue.ClosedAt = &now
	plan.Fields = []string{"state"}
	out.plan, out.move = plan, true
	out.fields, err = o.PrepareWorkItemFields(ctx, scope.Organization, plan)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(plan.Issue)
	if err != nil {
		return out, err
	}
	content := eventstore.NewContent(o.RunDetailStore())
	out.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, RecordID: item, Revision: int64(plan.Issue.Revision)}, raw)
	if err != nil {
		return out, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: plan.Issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, FromState: from, ToState: terminal, Reason: "worker_progress"}})
	if err != nil {
		return out, err
	}
	out.event, err = content.PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return out, err
	}
	return out, err
}

func (p IssueClosePlan) Write(ctx context.Context, w tenantstore.Writer) error {
	if !p.move {
		return nil
	}
	project, err := w.ProjectRecord(ctx, p.plan.Issue.ProjectID)
	if err != nil {
		return err
	}
	if !hubrecords.SameWorkItemProjectPlan(p.plan.Project, project) {
		return corestore.ErrSerializationFailure
	}
	current, err := w.LockWorkItem(ctx, p.plan.Issue.ProjectID, p.plan.Issue.WorkItemID)
	if err != nil {
		return err
	}
	if current.Revision != p.plan.Current.Revision || current.EventSequence != p.plan.Current.EventSequence {
		return corestore.ErrSerializationFailure
	}
	if err := hubrecords.WriteWorkItemRecord(ctx, w, p.plan, p.fields); err != nil {
		return err
	}
	if _, err := w.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if _, err := w.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	return nil
}

// workspaceIsolationRefused is the refusal a bind or heartbeat gets when the
// runner's terminal isolation is not one the organization allows.
func workspaceIsolationRefused() error {
	return &hubapi.NativeError{Code: "forbidden", Message: "The runner's terminal isolation is not allowed by this organization", Status: http.StatusForbidden, PublicMessage: true}
}

// FirstTerminalState reports the project's first terminal workflow state, the
// lane a closed workspace item is transitioned to.
func FirstTerminalState(project tracker.NativeProject) (string, bool) {
	for _, state := range project.States {
		if state.Terminal {
			return state.Name, true
		}
	}
	return "", false
}

// RequireUnreservedLabels refuses the workspace label on issue content that did
// not come from the hub. The label selects the runner's hold-open mode, which
// checks out the repository and keeps the claim until the workspace closes.
// A tracker writer who could set it would be able to
// park a runner on any issue indefinitely.
func RequireUnreservedLabels(ctx context.Context, labels []string) error {
	if LabelAllowed(ctx) {
		return nil
	}
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), ItemLabel) {
			return hubapi.NativePublicInvalid("Label " + ItemLabel + " is reserved")
		}
	}
	return nil
}
