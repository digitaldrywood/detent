package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func withMutableClaimFact(ctx context.Context, tx *sql.Tx, trigger string, write func() error) error {
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?`, trigger).Scan(&definition); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER `+trigger); err != nil {
		return err
	}
	if err := write(); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, definition)
	return err
}

func TestTypedRunnerAdmissionFacts(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
	expires := now.Add(time.Hour)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedStartupOrganization(t, repository, organization)
		observations := make(map[string]map[tracker.ProjectID]tracker.NativeAdmissionObservation)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for index := range 2 {
				runnerID := fmt.Sprintf("runner-%d", index)
				machineID := tracker.MachineID(fmt.Sprintf("machine-%d", index))
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machineID), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				hash := apikey.HashToken(string(organization) + runnerID)
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: runnerID, Name: runnerID, Hash: hash, Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "all", Kind: "service"}); err != nil {
					return err
				}
				enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + runnerID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("enrollment_" + string(organization) + runnerID), Scope: "organization", Operations: []string{"read", "claim"}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "admin"}
				if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, runnerID, machineID, now); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: machineID, TokenID: runnerID, EnrollmentID: enrollment.ID, Scope: "organization", Operations: enrollment.Operations, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}
				observations[runnerID] = make(map[tracker.ProjectID]tracker.NativeAdmissionObservation)
				for projectIndex := range 2 {
					project := tracker.ProjectID(fmt.Sprintf("project-%d", projectIndex))
					if index == 0 {
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
							return err
						}
					}
					observation := tracker.NativeAdmissionObservation{RunnerRevision: int64(index + 1), ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, LastRefreshAt: now, RefreshDuration: time.Millisecond, PolicyID: fmt.Sprintf("policy-%s-%d-%d", organization, index, projectIndex), WorkflowStates: []string{"Todo", "Rework"}, Authors: []string{"author-one", "author-two"}, Assignees: []string{"assignee-one"}, LabelInclude: []string{"include"}, LabelExclude: []string{"exclude"}}}
					observations[runnerID][project] = observation
					if err := w.ReplaceRunnerAdmissionObservation(t.Context(), runnerID, project, observation); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx := context.WithoutCancel(t.Context())
			if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				for runnerID, projects := range observations {
					for project, want := range projects {
						observed, err := r.RunnerAdmissionObservation(ctx, runnerID, project)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(observed, want) {
							return fmt.Errorf("organization=%s runner=%s project=%s observed=%+v want=%+v", organization, runnerID, project, observed, want)
						}
					}
				}
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestTypedAnsweredClaimFacts(t *testing.T) {
	for _, test := range []struct {
		name, status, reason, effect, state, want string
		present, human, blockers, acceptance      bool
	}{
		{name: "human", present: true, status: "blocked", human: true, want: "human_action"},
		{name: "permission", present: true, status: "blocked", reason: "permission_wait", want: "permission_wait"},
		{name: "recorded acceptance", acceptance: true, want: "recorded_acceptance"},
		{name: "continuing acceptance", present: true, status: "in_progress", acceptance: true},
		{name: "blocked acceptance", present: true, status: "in_progress", blockers: true, acceptance: true, want: "recorded_acceptance"},
		{name: "legacy push acceptance", effect: "git_push", state: "confirmed", acceptance: true},
		{name: "pending human", present: true, status: "blocked", human: true, effect: "git_push", state: "pending"},
		{name: "ambiguous permission", present: true, status: "blocked", reason: "permission_wait", effect: "pr_create", state: "ambiguous"},
		{name: "confirmed human", present: true, status: "blocked", human: true, effect: "git_push", state: "confirmed", want: "human_action"},
		{name: "plain success"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: test.acceptance}}}
			if test.present {
				data.Disposition = &tracker.NativeDisposition{Status: test.status, HumanAction: test.human, Blockers: test.blockers, ReasonCode: test.reason}
			}
			checkpoint := &tracker.NativeCheckpoint{ExternalEffect: test.effect, EffectState: test.state}
			data.Handoff = checkpoint
			if got := answeredClaimKind(data, checkpoint); got != test.want {
				t.Fatalf("classification=%q want %q", got, test.want)
			}
		})
	}
}
