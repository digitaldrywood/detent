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
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func testDashboardChangeLog(t *testing.T) {
	f := newNativeFixture(t, newPostgresNativeService(t, Config{Workspace: &WorkspaceConfig{Enabled: true}}), "", "Example project")
	f.service.localOrganization = string(f.project.OrganizationID)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "dashboard-other")
	fixtures := []nativeFixture{f, other}
	issues := []tracker.NativeIssue{f.create(t, "First dashboard item"), other.create(t, "Second dashboard item")}
	repository := f.service.config.TenantRepository
	snapshot := repository.(tenantstore.SnapshotRepository)
	ctx := t.Context()
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	scopes := []nativeScope{nativeBoardFixtureScope(t, f), nativeBoardFixtureScope(t, other)}
	head := func(scope nativeScope) viewstore.BoardSequence {
		t.Helper()
		var result viewstore.BoardSequence
		if err := repository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
			var err error
			result, err = r.BoardSequence(ctx, scope.project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	deltas := func(scope nativeScope, since int64) []viewstore.BoardDelta {
		t.Helper()
		var result []viewstore.BoardDelta
		if err := repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			result, err = r.BoardDeltas(ctx, scope.project, since, corestore.MaxPageSize)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	matching := func(scope nativeScope, since int64, topic, entity string) []viewstore.BoardDelta {
		var result []viewstore.BoardDelta
		for _, delta := range deltas(scope, since) {
			if delta.Topic == topic && delta.EntityID == entity {
				result = append(result, delta)
			}
		}
		return result
	}
	updateWorkspace := func(w tenantstore.Writer, scope nativeScope, id, state string) (postgres.WorkspaceSession, error) {
		owner := w.(postgres.WorkspaceWriter)
		current, err := owner.WorkspaceSession(ctx, scope.project, id)
		if err != nil {
			return postgres.WorkspaceSession{}, err
		}
		return owner.UpdateWorkspaceSession(ctx, postgres.WorkspaceUpdate{OrganizationID: scope.organization, ProjectID: scope.project, ID: id, ExpectedRevision: current.Revision, ExpectedRunnerID: current.RunnerID, ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken, RunnerID: current.RunnerID, MachineID: current.MachineID, LeaseID: current.LeaseID, FencingToken: current.FencingToken, State: state, UpdatedAt: now.Add(time.Second)})
	}
	command, err := f.service.runDetailStore().StoreDetail(ctx, string(f.project.OrganizationID), []byte("true"))
	if err != nil {
		t.Fatal(err)
	}
	for i, scope := range scopes {
		id := strconv.Itoa(i)
		if err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.(postgres.WorkspaceWriter).CreateWorkspaceSession(ctx, postgres.WorkspaceCreate{OrganizationID: scope.organization, ProjectID: scope.project, ID: dashboardWorkspaceID(id), SubjectWorkItemID: string(issues[i].WorkItemID), Ref: "develop", Requires: []string{"files"}, IdleTimeoutSeconds: 60, ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Minute), CreatedBy: "example-owner", CreatedAt: now}); err != nil {
				return err
			}
			if _, err := updateWorkspace(w, scope, dashboardWorkspaceID(id), "starting"); err != nil {
				return err
			}
			owner := w.(postgres.RelayActionWriter)
			if err := owner.CreateProjectAction(ctx, actionstore.Record{Action: workspacesession.Action{ID: "action_dashboard_" + id, OrganizationID: string(scope.organization), ProjectID: string(scope.project), Name: "Example action", Icon: "play", CreatedBy: "example-owner", Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: command}); err != nil {
				return err
			}
			_, err := owner.CreateActionRun(ctx, postgres.CreateActionRunInput{ID: "run_dashboard_" + id, OrganizationID: scope.organization, ProjectID: scope.project, ActionID: "action_dashboard_" + id, WorkspaceID: dashboardWorkspaceID(id), CreatedBy: "example-owner", CreatedAt: now})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	finding := healthFinding{Fingerprint: "dashboard", Signal: "example", Class: "flow", Severity: "attention", Summary: "Example finding", NextAction: "Inspect", Projects: []string{string(f.project.ID), string(other.project.ID)}}
	if err := f.service.publishHealthEvaluation(ctx, f.project.OrganizationID, now, []healthFinding{finding}); err != nil {
		t.Fatal(err)
	}
	var healthRecord postgres.HealthRecord
	if err := f.service.database.postgres.ReadHealth(ctx, f.project.OrganizationID, corestore.Serving, func(r postgres.HealthReader) error {
		var err error
		healthRecord, err = r.RecentHealthFingerprint(ctx, postgres.RecentHealthFingerprintInput{OrganizationID: f.project.OrganizationID, Fingerprint: "dashboard", ResolvedAt: new(now.Add(-healthReopenWindow))})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, f.service)
	t.Run("source creation produces one revision per project", func(t *testing.T) {
		for i, scope := range scopes {
			for _, test := range []struct{ topic, entity string }{{"work_item", string(issues[i].WorkItemID)}, {"human_action", string(issues[i].WorkItemID)}, {"workspace", dashboardWorkspaceID(strconv.Itoa(i))}, {"action_run", "run_dashboard_" + strconv.Itoa(i)}, {"health", healthRecord.ID}} {
				count := 0
				for _, delta := range matching(scope, 0, test.topic, test.entity) {
					if delta.EntityRevision != nil && *delta.EntityRevision == 1 {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("project %d %s creation rows=%d", i, test.topic, count)
				}
				if test.topic == "workspace" || test.topic == "action_run" {
					row := matching(scope, 0, test.topic, test.entity)[0]
					if row.WorkItemID != issues[i].WorkItemID {
						t.Fatalf("missing subject fence: %+v", row)
					}
					if err := repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
						frame, visible, err := f.service.dashboardRecordFrame(ctx, r, scope, &dashboardSubscription{}, row)
						if err != nil {
							return err
						}
						if !visible || frame.event != test.topic {
							t.Fatalf("source frame=%+v visible=%t", frame, visible)
						}
						reporter := scope
						reporter.credential.HostedRole = "reporter"
						reporter.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: "other-reporter"}
						_, visible, err = f.service.dashboardRecordFrame(ctx, r, reporter, &dashboardSubscription{}, row)
						if err == nil && visible {
							t.Fatal("reporter received another issue's workspace or action notice")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	})
	rollback := errors.New("rollback dashboard source")
	t.Run("workspace activity preserves lifecycle revisions and delivery", func(t *testing.T) {
		for i, scope := range scopes {
			id := dashboardWorkspaceID(strconv.Itoa(i))
			read := func() postgres.WorkspaceSession {
				t.Helper()
				var row postgres.WorkspaceSession
				if err := repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.(postgres.WorkspaceReader).WorkspaceSession(ctx, scope.project, id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return row
			}
			before := read()
			watermark := head(scope)
			at := now.Add(2 * time.Second)
			if err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(postgres.WorkspaceWriter)
				for _, observed := range []time.Time{at, at, now.Add(time.Second)} {
					if err := owner.TouchWorkspaceActivity(ctx, scope.project, id, observed); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			after := read()
			if after.Revision != before.Revision || !after.UpdatedAt.Equal(before.UpdatedAt) || after.LastActivityAt == nil || !after.LastActivityAt.Equal(at) {
				t.Fatalf("activity changed lifecycle or lost monotonic timestamp: before=%+v after=%+v", before, after)
			}
			if got := head(scope); got != watermark || len(matching(scope, watermark.Sequence, "workspace", id)) != 0 {
				t.Fatalf("activity published lifecycle delivery: before=%+v after=%+v", watermark, got)
			}
			err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.(postgres.WorkspaceWriter).TouchWorkspaceActivity(ctx, scope.project, id, at.Add(time.Second)); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			after = read()
			if after.Revision != before.Revision || !after.UpdatedAt.Equal(before.UpdatedAt) || after.LastActivityAt == nil || !after.LastActivityAt.Equal(at) || head(scope) != watermark {
				t.Fatalf("rolled-back activity escaped transaction: %+v", after)
			}
		}
	})
	contract, err := f.service.runDetailStore().StoreDetail(ctx, string(f.project.OrganizationID), []byte(`{"human_action":"Choose an option"}`))
	if err != nil {
		t.Fatal(err)
	}
	for i, scope := range scopes {
		id := strconv.Itoa(i)
		activityRaw, err := json.Marshal(tracker.CollaborationEvent{ID: "activity_dashboard_" + id, OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: issues[i].WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, RecordedAt: now, Actor: scope.actor(), Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped"}}})
		if err != nil {
			t.Fatal(err)
		}
		activity, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(ctx, scope.organization, activityRaw)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			topic, entity string
			revision      int64
			mutate        func(tenantstore.Writer) error
		}{
			{"work_item", string(issues[i].WorkItemID), 2, func(w tenantstore.Writer) error {
				item, err := w.WorkItem(ctx, scope.project, issues[i].WorkItemID)
				if err != nil {
					return err
				}
				item.Title = "Updated dashboard item"
				_, err = w.UpdateWorkItem(ctx, item)
				return err
			}},
			{"human_action", string(issues[i].WorkItemID), 3, func(w tenantstore.Writer) error {
				item, err := w.WorkItem(ctx, scope.project, issues[i].WorkItemID)
				if err != nil {
					return err
				}
				item.IssueContractObjectKey = &contract.Key
				_, err = w.UpdateWorkItem(ctx, item)
				return err
			}},
			{"lane", string(scope.project), -1, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(ctx, scope.project)
				if err != nil {
					return err
				}
				if len(project.States) == 0 {
					return errors.New("fixture has no workflow states")
				}
				project.States[0].OperatorOnly = !project.States[0].OperatorOnly
				_, err = w.UpdateProjectRecord(ctx, project)
				return err
			}},
			{"activity", "activity_dashboard_" + id, 0, func(w tenantstore.Writer) error {
				_, err := w.AppendEvent(ctx, activity)
				return err
			}},
			{"workspace", dashboardWorkspaceID(id), 3, func(w tenantstore.Writer) error {
				_, err := updateWorkspace(w, scope, dashboardWorkspaceID(id), "ready")
				return err
			}},
			{"action_run", "run_dashboard_" + id, 2, func(w tenantstore.Writer) error {
				if _, err := updateWorkspace(w, scope, dashboardWorkspaceID(id), "failed"); err != nil {
					return err
				}
				_, err := w.(postgres.RelayActionWriter).FailOrphanedActionRun(ctx, postgres.OrphanedActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: "run_dashboard_" + id, ExpectedRevision: 1, Now: now.Add(time.Second)})
				return err
			}},
		} {
			t.Run("source revisions commit once and roll back together/"+id+"/"+test.topic, func(t *testing.T) {
				before := head(scope).Sequence
				for _, commit := range []bool{false, true} {
					err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						if err := test.mutate(w); err != nil {
							return err
						}
						if head(scope).Sequence != before {
							t.Fatal("uncommitted source notice escaped transaction")
						}
						if !commit {
							return rollback
						}
						return nil
					})
					if !commit {
						if !errors.Is(err, rollback) {
							t.Fatal(err)
						}
						if got := len(matching(scope, before, test.topic, test.entity)); got != 0 {
							t.Fatalf("rolled-back rows=%d", got)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					rows := matching(scope, before, test.topic, test.entity)
					if len(rows) != 1 {
						t.Fatalf("committed rows=%d", len(rows))
					}
					if test.revision > 0 && (rows[0].EntityRevision == nil || *rows[0].EntityRevision != test.revision) {
						t.Fatalf("revision=%v want %d", rows[0].EntityRevision, test.revision)
					}
					if test.topic == "lane" && rows[0].Sequence <= before {
						t.Fatal("lane revision did not advance")
					}
				}
			})
		}
	}
	t.Run("health projects and revisions are atomic", func(t *testing.T) {
		before := []int64{head(scopes[0]).Sequence, head(scopes[1]).Sequence}
		for _, commit := range []bool{false, true} {
			err := f.service.database.postgres.WriteHealth(ctx, f.project.OrganizationID, corestore.Serving, func(w postgres.HealthWriter) error {
				_, err := w.UpdateHealthRecord(ctx, postgres.UpdateHealthRecordInput{OrganizationID: f.project.OrganizationID, ID: healthRecord.ID, ExpectedRevision: healthRecord.Revision, LastSeenAt: healthRecord.LastSeenAt, Severity: "watch", Summary: healthRecord.Summary, NextAction: healthRecord.NextAction, EvidenceObjectKey: healthRecord.EvidenceObjectKey, EvidenceSha256: healthRecord.EvidenceSha256, EvidenceBytes: healthRecord.EvidenceBytes, EvidenceContentBytes: healthRecord.EvidenceContentBytes})
				if err != nil {
					return err
				}
				for i, scope := range scopes {
					if head(scope).Sequence != before[i] {
						t.Fatal("health head escaped transaction")
					}
				}
				if !commit {
					return rollback
				}
				return nil
			})
			if !commit && !errors.Is(err, rollback) || commit && err != nil {
				t.Fatal(err)
			}
			for i, scope := range scopes {
				want := 0
				if commit {
					want = 1
				}
				if got := len(matching(scope, before[i], "health", healthRecord.ID)); got != want {
					t.Fatalf("health rows=%d want %d", got, want)
				}
			}
		}
	})
	t.Run("health writer routes membership changes once per revision", func(t *testing.T) {
		service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
		first := newNativeFixture(t, service, "org_first", "Example first project")
		second := newNativeFixture(t, service, first.project.OrganizationID, "Example second project")
		organization := first.project.OrganizationID
		projects := []tracker.ProjectID{first.project.ID, second.project.ID}
		repository := service.database.postgres
		now := service.config.now().UTC().Truncate(time.Microsecond)
		head := func(project tracker.ProjectID) int64 {
			t.Helper()
			var sequence int64
			err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				watermark, err := r.BoardSequence(t.Context(), project)
				sequence = watermark.Sequence
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return sequence
		}
		readDeltas := func(project tracker.ProjectID, after int64) []viewstore.BoardDelta {
			t.Helper()
			var deltas []viewstore.BoardDelta
			err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				deltas, err = r.BoardDeltas(t.Context(), project, after, corestore.MaxPageSize)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			return deltas
		}
		finding := healthFinding{Fingerprint: "dashboard-writer", Signal: "example", Class: "flow", Severity: "watch", Summary: "Example finding", NextAction: "Inspect", Projects: []string{string(first.project.ID), string(second.project.ID)}}
		var current postgres.HealthRecord
		rollback := fmt.Errorf("rollback health mutation")
		for step, test := range []struct {
			projects []string
			kinds    [2]string
			resolved bool
		}{
			{finding.Projects, [2]string{"upsert", "upsert"}, false},
			{finding.Projects, [2]string{"upsert", "upsert"}, false},
			{[]string{string(second.project.ID)}, [2]string{"delete", "upsert"}, false},
			{[]string{}, [2]string{"", "delete"}, false},
			{finding.Projects, [2]string{"upsert", "upsert"}, false},
			{finding.Projects, [2]string{"upsert", "upsert"}, true},
		} {
			before := []int64{head(projects[0]), head(projects[1])}
			findings := []healthFinding{finding}
			findings[0].Projects = test.projects
			if test.resolved {
				findings = nil
			}
			if err := service.publishHealthEvaluation(t.Context(), organization, now.Add(time.Duration(step)*time.Second), findings); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadHealth(t.Context(), organization, corestore.Serving, func(r postgres.HealthReader) error {
				var err error
				current, err = r.RecentHealthFingerprint(t.Context(), postgres.RecentHealthFingerprintInput{OrganizationID: organization, Fingerprint: finding.Fingerprint, ResolvedAt: new(now.Add(-healthReopenWindow))})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for i, project := range projects {
				var matching []viewstore.BoardDelta
				for _, delta := range readDeltas(project, before[i]) {
					if delta.Topic == "health" && delta.EntityID == current.ID {
						matching = append(matching, delta)
					}
				}
				want := 1
				if test.kinds[i] == "" {
					want = 0
				}
				if len(matching) != want {
					t.Fatalf("step %d project %d rows=%d want %d", step, i, len(matching), want)
				}
				if want == 1 {
					delta := matching[0]
					if delta.Kind != test.kinds[i] || delta.EntityRevision == nil || *delta.EntityRevision != int64(step+1) || test.resolved && delta.Status != "resolved" {
						t.Fatalf("step %d project %d change=%+v", step, i, delta)
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						scope := nativeScope{organization: organization, project: project, credential: apiCredential{Scope: apiScopeAdmin}}
						frame, visible, err := service.dashboardRecordFrame(t.Context(), r, scope, &dashboardSubscription{}, delta)
						if err == nil && (!visible || frame.event != "health.findings") {
							t.Fatalf("health frame=%+v visible=%t", frame, visible)
						}
						if err != nil {
							return err
						}
						scope.credential.HostedRole = "reporter"
						_, visible, err = service.dashboardRecordFrame(t.Context(), r, scope, &dashboardSubscription{}, delta)
						if err == nil && visible {
							t.Fatal("reporter received a project health finding event")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			committed := []int64{head(projects[0]), head(projects[1])}
			err := repository.WriteHealth(t.Context(), organization, corestore.Background, func(w postgres.HealthWriter) error {
				_, err := w.UpdateHealthRecord(t.Context(), postgres.UpdateHealthRecordInput{OrganizationID: organization, ID: current.ID, ExpectedRevision: current.Revision, LastSeenAt: current.LastSeenAt, ResolvedAt: current.ResolvedAt, Severity: current.Severity, Summary: "Uncommitted finding", NextAction: current.NextAction, EvidenceObjectKey: current.EvidenceObjectKey, EvidenceSha256: current.EvidenceSha256, EvidenceBytes: current.EvidenceBytes, EvidenceContentBytes: current.EvidenceContentBytes})
				if err != nil {
					return err
				}
				if err := w.ReplaceHealthProjectLinks(t.Context(), postgres.ReplaceHealthProjectLinksInput{OrganizationID: organization, FindingID: current.ID}); err != nil {
					return err
				}
				for i, project := range projects {
					if head(project) != committed[i] {
						t.Fatal("health writer exposed an uncommitted event")
					}
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("health rollback=%v", err)
			}
			for i, project := range projects {
				if head(project) != committed[i] {
					t.Fatal("health rollback advanced the project head")
				}
			}
			if err := repository.ReadHealth(t.Context(), organization, corestore.Serving, func(r postgres.HealthReader) error {
				row, err := r.HealthRecord(t.Context(), postgres.HealthRecordInput{OrganizationID: organization, ID: current.ID})
				if err == nil && (row.Revision != current.Revision || row.Summary != current.Summary) {
					t.Fatalf("rollback changed finding=%+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		before := []int64{head(projects[0]), head(projects[1])}
		if err := repository.WriteHealth(t.Context(), organization, corestore.Background, func(w postgres.HealthWriter) error {
			var err error
			current, err = w.UpdateHealthRecord(t.Context(), postgres.UpdateHealthRecordInput{OrganizationID: organization, ID: current.ID, ExpectedRevision: current.Revision, LastSeenAt: now.Add(6 * time.Second), Severity: current.Severity, Summary: current.Summary, NextAction: current.NextAction, EvidenceObjectKey: current.EvidenceObjectKey, EvidenceSha256: current.EvidenceSha256, EvidenceBytes: current.EvidenceBytes, EvidenceContentBytes: current.EvidenceContentBytes})
			if err != nil {
				return err
			}
			if err := w.ReplaceHealthProjectLinks(t.Context(), postgres.ReplaceHealthProjectLinksInput{OrganizationID: organization, FindingID: current.ID, ProjectIds: []string{string(second.project.ID)}}); err != nil {
				return err
			}
			return w.LinkHealthProject(t.Context(), postgres.LinkHealthProjectInput{OrganizationID: organization, FindingID: current.ID, ProjectID: first.project.ID})
		}); err != nil {
			t.Fatal(err)
		}
		for i, project := range projects {
			rows := readDeltas(project, before[i])
			if len(rows) != 1 || rows[0].EntityID != current.ID || rows[0].Kind != "upsert" || rows[0].EntityRevision == nil || *rows[0].EntityRevision != 7 || rows[0].Status != "watch" {
				t.Fatalf("remove and re-add did not publish final membership: %+v", rows)
			}
		}
		before = []int64{head(projects[0]), head(projects[1])}
		replacementID := newNativeID("finding")
		if err := repository.WriteHealth(t.Context(), organization, corestore.Background, func(w postgres.HealthWriter) error {
			_, err := w.UpdateHealthRecord(t.Context(), postgres.UpdateHealthRecordInput{OrganizationID: organization, ID: current.ID, ExpectedRevision: current.Revision, LastSeenAt: current.LastSeenAt, ResolvedAt: new(now.Add(7 * time.Second)), Severity: current.Severity, Summary: current.Summary, NextAction: current.NextAction, EvidenceObjectKey: current.EvidenceObjectKey, EvidenceSha256: current.EvidenceSha256, EvidenceBytes: current.EvidenceBytes, EvidenceContentBytes: current.EvidenceContentBytes})
			if err != nil {
				return err
			}
			_, err = w.CreateHealthRecord(t.Context(), postgres.CreateHealthRecordInput{OrganizationID: organization, ID: replacementID, Fingerprint: finding.Fingerprint, Signal: current.Signal, Class: current.Class, SubjectKind: current.SubjectKind, SubjectID: current.SubjectID, OpenedAt: now.Add(2 * time.Hour), LastSeenAt: now.Add(2 * time.Hour), Severity: current.Severity, Summary: current.Summary, NextAction: current.NextAction, EvidenceObjectKey: current.EvidenceObjectKey, EvidenceSha256: current.EvidenceSha256, EvidenceBytes: current.EvidenceBytes, EvidenceContentBytes: current.EvidenceContentBytes})
			if err != nil {
				return err
			}
			return w.LinkHealthProject(t.Context(), postgres.LinkHealthProjectInput{OrganizationID: organization, FindingID: replacementID, ProjectID: first.project.ID})
		}); err != nil {
			t.Fatal(err)
		}
		rows := readDeltas(first.project.ID, before[0])
		if len(rows) != 2 || rows[0].EntityID != current.ID || rows[0].Status != "resolved" || rows[0].EntityRevision == nil || *rows[0].EntityRevision != 8 || rows[1].EntityID != replacementID || rows[1].Kind != "upsert" || rows[1].EntityRevision == nil || *rows[1].EntityRevision != 1 {
			t.Fatalf("replacement episode hid a distinct finding: %+v", rows)
		}
		if rows := readDeltas(second.project.ID, before[1]); len(rows) != 1 || rows[0].EntityID != current.ID || rows[0].Status != "resolved" {
			t.Fatalf("old episode resolution lost its other project: %+v", rows)
		}
	})
	t.Run("serialized commits and read snapshots", func(t *testing.T) {
		before := []int64{head(scopes[0]).Sequence, head(scopes[1]).Sequence}
		ready := []chan struct{}{make(chan struct{}), make(chan struct{})}
		release := []chan struct{}{make(chan struct{}), make(chan struct{})}
		finished := []chan struct{}{make(chan struct{}), make(chan struct{})}
		results := []chan error{make(chan error, 1), make(chan error, 1)}
		once := []sync.Once{{}, {}}
		t.Cleanup(func() {
			for i := range release {
				once[i].Do(func() { close(release[i]) })
			}
			for _, done := range finished {
				<-done
			}
		})
		for i, scope := range scopes {
			go func() {
				defer close(finished[i])
				signaled := false
				err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(ctx, scope.project, issues[i].WorkItemID)
					if err != nil {
						return err
					}
					item.Title = []string{"First writer", "Second writer"}[i]
					if _, err := w.UpdateWorkItem(ctx, item); err != nil {
						return err
					}
					close(ready[i])
					signaled = true
					<-release[i]
					return nil
				})
				if !signaled {
					close(ready[i])
				}
				results[i] <- err
			}()
		}
		for _, signal := range ready {
			<-signal
		}
		for i, scope := range scopes {
			if head(scope).Sequence != before[i] {
				t.Fatal("uncommitted writer became visible")
			}
		}
		once[0].Do(func() { close(release[0]) })
		if err := <-results[0]; err != nil {
			t.Fatal(err)
		}
		if err := snapshot.ReadTenantSnapshot(ctx, scopes[0].organization, corestore.Serving, func(r tenantstore.Reader) error {
			watermark, err := r.BoardSequence(ctx, scopes[0].project)
			if err != nil {
				return err
			}
			if watermark.Sequence != before[0]+1 {
				t.Fatalf("visible watermark=%d want %d", watermark.Sequence, before[0]+1)
			}
			once[1].Do(func() { close(release[1]) })
			if err := <-results[1]; err != nil {
				return err
			}
			item, err := r.WorkItem(ctx, scopes[1].project, issues[1].WorkItemID)
			if err != nil {
				return err
			}
			if item.Revision != 3 || item.Title != "Updated dashboard item" {
				t.Fatalf("read snapshot=%+v", item)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for i, scope := range scopes {
			rows := matching(scope, before[i], "work_item", string(issues[i].WorkItemID))
			if len(rows) != 1 || rows[0].EntityRevision == nil || *rows[0].EntityRevision != 4 || head(scope).Sequence != before[i]+1 {
				t.Fatalf("project %d committed revisions=%+v", i, rows)
			}
		}
	})
	t.Run("dashboard snapshot pins its data and watermark", func(t *testing.T) {
		drainTestNativeBoard(t, f.service)
		before := head(scopes[0]).Sequence
		arrived, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var pause, resume sync.Once
		t.Cleanup(func() { resume.Do(func() { close(release) }); <-finished })
		readContext := postgres.WithTransactionTiming(ctx, nil, func(_ time.Time, query string) {
			if strings.Contains(query, "FROM projects") {
				pause.Do(func() { close(arrived); <-release })
			}
		})
		type result struct {
			page tracker.NativeIssuePage
			err  error
		}
		results := make(chan result, 1)
		go func() {
			defer close(finished)
			page, err := f.service.readBoardRecords(readContext, scopes[0], url.Values{})
			results <- result{page, err}
		}()
		select {
		case <-arrived:
		case result := <-results:
			t.Fatalf("snapshot did not reach source read: %v", result.err)
		}
		if err := repository.WriteTenant(ctx, scopes[0].organization, corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(ctx, scopes[0].project, issues[0].WorkItemID)
			if err != nil {
				return err
			}
			item.Title = "After snapshot"
			item.Archived = true
			_, err = w.UpdateWorkItem(ctx, item)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		drainTestNativeBoard(t, f.service)
		resume.Do(func() { close(release) })
		got := <-results
		if got.err != nil || got.page.ChangeSequence != before || len(got.page.Items) != 1 || got.page.Items[0].Revision != 4 || got.page.Items[0].Title != "First writer" {
			t.Fatalf("snapshot mixed revisions: page=%+v error=%v", got.page, got.err)
		}
		if head(scopes[0]).Sequence <= got.page.ChangeSequence {
			t.Fatal("concurrent write did not advance durable head")
		}
	})
	t.Run("dashboard surface watermarks share their source snapshot", func(t *testing.T) {
		for i, scope := range scopes {
			raw, err := f.service.readWorkspaces(ctx, scope, "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			var workspaces workspaceListResponse
			if err := json.Unmarshal(raw, &workspaces); err != nil {
				t.Fatal(err)
			}
			if workspaces.ChangeSequence != head(scope).Sequence || len(workspaces.Workspaces) != 1 || workspaces.Workspaces[0].Revision != 4 || workspaces.Workspaces[0].State != "failed" {
				t.Fatalf("workspace snapshot=%+v", workspaces)
			}
			raw, err = f.service.readActionRuns(ctx, scope, "action_dashboard_"+strconv.Itoa(i), 100)
			if err != nil {
				t.Fatal(err)
			}
			var runs projectActionRunList
			if err := json.Unmarshal(raw, &runs); err != nil {
				t.Fatal(err)
			}
			if runs.ChangeSequence != head(scope).Sequence || len(runs.Items) != 1 || runs.Items[0].Revision != 2 || runs.Items[0].Status != "failed" {
				t.Fatalf("action snapshot=%+v", runs)
			}
			health, err := f.service.readHealthFindings(ctx, scope, "all", "", "", 100)
			if err != nil || health.ChangeSequence != head(scope).Sequence {
				t.Fatalf("health snapshot=%+v: %v", health, err)
			}
			runtime, err := f.service.readNativeRuntime(ctx, scope, string(issues[i].WorkItemID), "")
			if err != nil || runtime.ChangeSequence != head(scope).Sequence {
				t.Fatalf("runtime watermark=%d: %v", runtime.ChangeSequence, err)
			}
		}
	})
	t.Run("human attempt dispositions replay idempotently", func(t *testing.T) {
		for i, fixture := range fixtures {
			approveHubTestPolicy(t, fixture.service, fixture.base+"/policy", hubTestPolicy())
			issue := fixture.create(t, "Human question source")
			worker := fixture.worker(t, "dashboard-human-"+strconv.Itoa(i))
			lease := claimNativeAttempt(t, fixture, worker, "example-machine-"+strconv.Itoa(i), "example-session-"+strconv.Itoa(i), issue.WorkItemID)
			path := fixture.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			event := nativeStartedEvent(lease)
			event.IdempotencyKey = "dashboard-human-start"
			requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path, worker, event), http.StatusOK)
			before := head(scopes[i]).Sequence
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "dashboard-human-finish", 2, "succeeded"
			event.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true}
			for range 2 {
				requireNativeStatus(t, performHubAPIRequest(t, fixture.service, http.MethodPost, path, worker, event), http.StatusOK)
			}
			rows := matching(scopes[i], before, "human_action", event.Data.AttemptID)
			if len(rows) != 1 || rows[0].EntityRevision == nil || *rows[0].EntityRevision != 2 || rows[0].Status != "blocked" || rows[0].RelatedID != string(issue.WorkItemID) {
				t.Fatalf("human disposition=%+v", rows)
			}
		}
		for _, queued := range []bool{false, true} {
			t.Run(fmt.Sprintf("typed owner queued=%t", queued), func(t *testing.T) {
				fixture := fixtures[0]
				scope := scopes[0]
				name := fmt.Sprintf("dashboard-typed-%t", queued)
				issue := fixture.create(t, name)
				worker := fixture.worker(t, name)
				lease := claimNativeAttempt(t, fixture, worker, name+"-machine", name+"-session", issue.WorkItemID)
				data := nativeStartedEvent(lease).Data
				for _, sequence := range []int64{1, 2} {
					data.Sequence = sequence
					status := "running"
					if sequence == 2 {
						status = "succeeded"
						data.Outcome = "succeeded"
						data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true}
					}
					raw, err := json.Marshal(data)
					if err != nil {
						t.Fatal(err)
					}
					ref, err := fixture.service.runDetailStore().StoreDetail(ctx, string(scope.organization), raw)
					if err != nil {
						t.Fatal(err)
					}
					event := leasestore.AttemptEvent{ID: data.AttemptID, RunID: data.RunID, Fence: leasestore.Fence{LeaseID: string(data.LeaseID), FencingToken: int64(data.FencingToken)}, Sequence: sequence, Status: status, PayloadObjectKey: ref.Key, PayloadReference: ref}
					original, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					if sequence == 2 {
						event.HumanActionStatus = new("blocked")
					}
					projected, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					if string(original) != string(projected) {
						t.Fatal("human projection changed receipt identity")
					}
					before := head(scope).Sequence
					for replay := range 2 {
						if err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
							record := func(w tenantstore.Writer) error {
								_, duplicate, err := w.RecordAttemptEvent(ctx, event)
								if err == nil && duplicate != (replay == 1) {
									t.Fatalf("duplicate=%t replay=%d", duplicate, replay)
								}
								return err
							}
							if !queued {
								return record(w)
							}
							return w.(interface {
								WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
							}).WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: scope.project, WorkItem: issue.WorkItemID, LeaseID: string(data.LeaseID), AttemptID: data.AttemptID, Operation: "dashboard-human-projection", Sequences: []int64{sequence}}, record)
						}); err != nil {
							t.Fatal(err)
						}
					}
					rows := matching(scope, before, "human_action", data.AttemptID)
					if sequence == 1 && len(rows) != 0 {
						t.Fatalf("non-human start emitted notices: %+v", rows)
					}
					if sequence == 2 && (len(rows) != 1 || rows[0].EntityRevision == nil || *rows[0].EntityRevision != 2 || rows[0].Status != "blocked" || rows[0].RelatedID != string(issue.WorkItemID)) {
						t.Fatalf("typed human projection=%+v", rows)
					}
				}
			})
		}
	})
	t.Run("deletion commits its tombstone", func(t *testing.T) {
		for i, scope := range scopes {
			before := head(scope).Sequence
			id := "run_dashboard_" + strconv.Itoa(i)
			for _, commit := range []bool{false, true} {
				err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					owner := w.(postgres.RelayActionWriter)
					if err := owner.DeleteActionRun(ctx, scope.project, id, 2); err != nil {
						return err
					}
					if _, err := owner.ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: id}); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("deleted row remains inside transaction: %v", err)
					}
					if len(matching(scope, before, "action_run", id)) != 0 {
						t.Fatal("tombstone escaped transaction")
					}
					if !commit {
						return rollback
					}
					return nil
				})
				if !commit && !errors.Is(err, rollback) || commit && err != nil {
					t.Fatal(err)
				}
			}
			rows := matching(scope, before, "action_run", id)
			if len(rows) != 1 || rows[0].Kind != "delete" {
				t.Fatalf("tombstone=%+v", rows)
			}
			if err := repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(postgres.RelayActionReader).ActionRun(ctx, postgres.ActionRunInput{OrganizationID: scope.organization, ProjectID: scope.project, ID: id})
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("deleted row still exists: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("retention progresses on unchanged projects with an inclusive floor", func(t *testing.T) {
		drainTestNativeBoard(t, f.service)
		admin := usagePostgresAdmin(t, f.service)
		plan, err := admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Rollback()
		if _, err := plan.ExecContext(ctx, `SET LOCAL enable_seqscan=off`); err != nil {
			t.Fatal(err)
		}
		rows, err := plan.QueryContext(ctx, `EXPLAIN SELECT project_id FROM native_board_sequences WHERE organization_id=$1 AND project_id>$2 ORDER BY project_id LIMIT 1 FOR UPDATE SKIP LOCKED`, scopes[0].organization, "")
		if err != nil {
			t.Fatal(err)
		}
		var explanation strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			explanation.WriteString(line)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(explanation.String(), "native_board_sequences_pkey") {
			t.Fatalf("retention discovery is not an indexed seek: %s", explanation.String())
		}
		if err := plan.Rollback(); err != nil {
			t.Fatal(err)
		}
		for _, scope := range scopes {
			before := head(scope).Sequence
			added := int64(nativeBoardReplayLimit + 2*nativeBoardTrimLimit)
			if _, err := admin.ExecContext(ctx, `INSERT INTO native_board_deltas(organization_id,project_id,sequence,work_item_id,topic,entity_id,entity_revision,kind) SELECT $1,$2,n,'','lane',$2,1,'invalidate' FROM generate_series($3::bigint,$4::bigint) n`, scope.organization, scope.project, before+1, before+added); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(ctx, `UPDATE native_board_sequences SET sequence=$3 WHERE organization_id=$1 AND project_id=$2`, scope.organization, scope.project, before+added); err != nil {
				t.Fatal(err)
			}
		}
		for _, scope := range scopes {
			var beforeCount, floor int64
			beforeHead := head(scope).Sequence
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM native_board_deltas WHERE organization_id=$1 AND project_id=$2`, scope.organization, scope.project).Scan(&beforeCount); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(ctx, `SELECT sequence FROM native_board_deltas WHERE organization_id=$1 AND project_id=$2 ORDER BY sequence OFFSET $3 LIMIT 1`, scope.organization, scope.project, nativeBoardTrimLimit-1).Scan(&floor); err != nil {
				t.Fatal(err)
			}
			f.service.boardRetentionScope = nativeScope{}
			for range len(scopes) + 1 {
				f.service.maintainNativeBoard(ctx)
				if f.service.boardRetentionScope.project == scope.project {
					break
				}
			}
			var afterCount int64
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM native_board_deltas WHERE organization_id=$1 AND project_id=$2`, scope.organization, scope.project).Scan(&afterCount); err != nil {
				t.Fatal(err)
			}
			if beforeCount-afterCount != nativeBoardTrimLimit {
				t.Fatalf("trim removed %d want %d", beforeCount-afterCount, nativeBoardTrimLimit)
			}
			watermark := head(scope)
			if watermark.Sequence != beforeHead || watermark.ReplayFloor != floor {
				t.Fatalf("watermarks=%+v want (%d,%d)", watermark, beforeHead, floor)
			}
			for _, since := range []int64{floor - 1, floor} {
				sub := &dashboardSubscription{initial: true, sequence: since, frames: make(chan dashboardFrame, 2), done: make(chan struct{})}
				delivery := &dashboardDelivery{config: dashboardDeliveryConfig{pageSize: 1}}
				more, err := f.service.dashboardProjectRecordPage(ctx, delivery, sub, scope)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case frame := <-sub.frames:
					if (frame.event == "reset") != (since < floor) {
						t.Fatalf("inclusive floor since=%d frame=%+v", since, frame)
					}
					if since == floor && (!more || sub.sequence != floor+1 || sub.sequence >= beforeHead) {
						t.Fatalf("bounded replay skipped unread tail: %+v", sub)
					}
				default:
					t.Fatal("replay produced no frame")
				}
			}
			if head(scope).Sequence != beforeHead {
				t.Fatal("retention generated a tombstone")
			}
		}
	})
	t.Run("empty feed starts at zero", func(t *testing.T) {
		var project corestore.Project
		if err := repository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			project, err = w.CreateProject(ctx, corestore.Project{OrganizationID: f.project.OrganizationID, ID: "prj_empty_dashboard", Name: "Empty dashboard", Slug: "empty-dashboard", Profile: "native"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: f.project.OrganizationID, project: project.ID}
		watermark := head(scope)
		if watermark.Sequence != 0 || watermark.ReplayFloor != 0 || len(deltas(scope, 0)) != 0 {
			t.Fatalf("empty feed=%+v", watermark)
		}
	})
}

func testDashboardProjectionAfterSourceSnapshot(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	issue := f.create(t, "First dashboard item")
	drainTestNativeBoard(t, f.service)
	scope := nativeBoardFixtureScope(t, f)
	ctx := t.Context()
	repository := f.service.config.TenantRepository
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	for revision := int64(1); revision <= 10; revision++ {
		title := "Dashboard item revision " + strconv.FormatInt(revision+1, 10)
		if revision == 10 {
			title = "Updated dashboard item"
		}
		response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{
			Mutation:         tracker.Mutation{IdempotencyKey: "dashboard-edit-" + strconv.FormatInt(revision, 10)},
			ExpectedRevision: tracker.Revision(revision), Title: &title,
		})
		requireNativeStatus(t, response, http.StatusOK)
	}
	readHead := func() viewstore.BoardSequence {
		t.Helper()
		var head viewstore.BoardSequence
		if err := repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			head, err = r.BoardSequence(ctx, scope.project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return head
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=board", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.NativeIssuePage
	decodeHubResponse(t, response, &page)
	before := readHead()
	if page.ChangeSequence != before.Sequence {
		t.Fatalf("board watermark=%d head=%d", page.ChangeSequence, before.Sequence)
	}
	abort := errors.New("rollback board projection")
	err := repository.WriteTenant(ctx, scope.organization, corestore.Background, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(ctx, scope.project, issue.WorkItemID)
		if err != nil {
			return err
		}
		project, err := w.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if err := w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{
			ProjectID: scope.project, WorkItemID: issue.WorkItemID,
			ExpectedRevision: item.Revision, ExpectedEventSequence: item.EventSequence,
			ExpectedProjectRevision: project.ContentSettingsRevision,
		}); err != nil {
			return err
		}
		if _, err := w.BoardItem(ctx, scope.project, issue.WorkItemID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("projection removal did not take effect inside the transaction: %v", err)
		}
		if head := readHead(); head != before {
			t.Fatalf("projection feed escaped its transaction: head=%+v before=%+v", head, before)
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if head := readHead(); head != before {
		t.Fatalf("projection rollback advanced head: head=%+v before=%+v", head, before)
	}
	drainTestNativeBoard(t, f.service)
	frame := readTestBoardRecordFrame(t, f.service, scope, before.Sequence)
	if frame.Gap || len(frame.Deltas) != 1 || frame.Deltas[0].Current == nil ||
		frame.Deltas[0].Current.Issue.Title != "Updated dashboard item" || frame.Deltas[0].Sequence <= before.Sequence {
		t.Fatalf("later projection=%#v", frame)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot tracker.NativeIssue
	decodeHubResponse(t, response, &snapshot)
	if snapshot.ChangeSequence != readHead().Sequence || snapshot.Revision != 11 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func dashboardWorkspaceID(id string) string {
	return "ws_" + strings.Repeat("0", 32-len(id)) + id
}
