package runnerfleet_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTypedRunnerAdmissionFacts(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
	expires := now.Add(time.Hour)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: now})
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
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: hubtest.States()}); err != nil {
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
