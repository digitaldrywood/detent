package hubserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) reportsTrackingIssues(ctx context.Context, scope hubscope.NativeScope, classes []reportClassTotal, failures []nativeFailure) error {
	if len(failures) > maxAnalyticsPopulation {
		return hubapi.NativePublicInvalid("Report failure tracking population exceeds the read limit")
	}
	fingerprints := reportsTrackingFingerprints(failures)
	if len(fingerprints) == 0 {
		return nil
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	tracking := map[string]corestore.DoctorProposalIdentityResult{}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		keys := make([]string, 0, len(fingerprints))
		for fingerprint := range fingerprints {
			keys = append(keys, fingerprint)
		}
		slices.Sort(keys)
		for _, fingerprint := range keys {
			class := fingerprints[fingerprint]
			result, err := r.LookupDoctorProposalIdentity(ctx, corestore.DoctorProposalIdentitySelection{ProjectID: scope.Project, Identity: corestore.DoctorProposalIdentity{Kind: "fingerprint", Value: fingerprint}, OpenOnly: true, Earliest: true})
			if err != nil {
				return err
			}
			if result.WorkItemID == "" {
				continue
			}
			current, found := tracking[class]
			if !found || result.Number < current.Number || result.Number == current.Number && result.WorkItemID < current.WorkItemID {
				tracking[class] = result
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range classes {
		classes[i].TrackingWorkItemID = string(tracking[classes[i].Class].WorkItemID)
	}
	return nil
}

func (s *Service) populatePostgresReportTracking(ctx context.Context, credential hubapi.APICredential, projects []string, attempts []reportstore.AttemptFact, w operatortool.AnalyticsWindow, out *hostedReportsReport) (bool, error) {
	selected := []reportstore.AttemptFact{}
	for _, attempt := range attempts {
		if attempt.Failed && !attempt.OutcomeAt.Before(w.From) && attempt.OutcomeAt.Before(w.To) {
			selected = append(selected, attempt)
		}
	}
	if len(selected) == 0 {
		return false, nil
	}
	slices.SortFunc(selected, func(a, b reportstore.AttemptFact) int {
		return cmp.Or(b.OutcomeAt.Compare(a.OutcomeAt), strings.Compare(a.AttemptID, b.AttemptID))
	})
	partial := len(selected) > maxAnalyticsPopulation
	selected = selected[:min(len(selected), maxAnalyticsPopulation)]
	events := make([]eventstore.Event, len(selected))
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(hubrecords.NativeRunEventReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		authorized := map[tracker.ProjectID]bool{}
		for index, attempt := range selected {
			if !authorized[attempt.ProjectID] {
				if !slices.Contains(projects, string(attempt.ProjectID)) {
					return hubapi.NativeNotFound()
				}
				if err := s.authorizeAccessScope(ctx, r, hubscope.NativeScope{Organization: organization, Project: attempt.ProjectID, Credential: credential}, false); err != nil {
					return err
				}
				authorized[attempt.ProjectID] = true
			}
			if !attempt.ProjectionKnown || attempt.EventID == "" {
				partial = true
				continue
			}
			event, err := reader.RunEvent(ctx, attempt.ProjectID, attempt.WorkItemID, attempt.EventID)
			if errors.Is(err, corestore.ErrNotFound) {
				partial = true
				continue
			}
			if err != nil {
				return err
			}
			if event.AttemptID != attempt.AttemptID || event.ProjectID != attempt.ProjectID || event.WorkItemID != attempt.WorkItemID || event.OrganizationID != organization {
				return artifact.ErrIntegrity
			}
			events[index] = event
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read tracking failure events: %w", err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	values, readErrors := readDiagnosticObjects(events, func(event eventstore.Event) (tracker.CollaborationEvent, error) {
		if event.ID == "" {
			return tracker.CollaborationEvent{}, artifact.ErrMissing
		}
		return content.Event(ctx, event)
	})
	failures := map[tracker.ProjectID][]nativeFailure{}
	for index, value := range values {
		if err := readErrors[index]; errors.Is(err, fs.ErrNotExist) || errors.Is(err, artifact.ErrMissing) {
			partial = true
			continue
		} else if err != nil {
			return false, fmt.Errorf("read tracking failure object: %w", err)
		}
		attempt := selected[index]
		if value.Data.Run == nil || value.Data.Run.AttemptID != attempt.AttemptID || value.AggregateID != attempt.WorkItemID || value.ProjectID != attempt.ProjectID || value.OrganizationID != organization {
			return false, artifact.ErrIntegrity
		}
		example, class := failureFromAttempt(*value.Data.Run, attempt.Status)
		if example == "" || classifyFailure(example).Name != attempt.FailureClass {
			partial = true
			continue
		}
		failures[attempt.ProjectID] = append(failures[attempt.ProjectID], nativeFailure{AttemptID: attempt.AttemptID, WorkItem: string(attempt.WorkItemID), At: attempt.OutcomeAt, Example: example, Class: class})
	}
	for _, project := range projects {
		projectFailures := failures[tracker.ProjectID(project)]
		if len(projectFailures) == 0 {
			continue
		}
		classes := reportFailureClasses(projectFailures)
		if err := s.reportsTrackingIssues(ctx, hubscope.NativeScope{Organization: organization, Project: tracker.ProjectID(project), Credential: credential}, classes, projectFailures); errors.Is(err, postgres.ErrDoctorProposalIdentitiesPending) {
			partial = true
			continue
		} else if err != nil {
			return false, err
		}
		for _, class := range classes {
			for i := range out.Failures {
				if out.Failures[i].Class == class.Class && out.Failures[i].TrackingWorkItemID == "" {
					out.Failures[i].TrackingWorkItemID = class.TrackingWorkItemID
				}
			}
		}
	}
	return partial, nil
}
