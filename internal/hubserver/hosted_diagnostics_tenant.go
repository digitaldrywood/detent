package hubserver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readCloudDiagnosticsTenant(ctx context.Context, credential hubapi.APICredential, w operatortool.AnalyticsWindow) (diagnosticsReport, error) {
	return s.readDiagnosticsTenant(ctx, credential, w, "")
}

func (s *Service) readDiagnosticsTenant(ctx context.Context, credential hubapi.APICredential, w operatortool.AnalyticsWindow, requested tracker.ProjectID) (diagnosticsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return diagnosticsReport{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	ctx = s.runDetailContext(ctx)
	var ids []tracker.ProjectID
	var err error
	projectPartial := false
	principal := ""
	if credential.HostedRole == "reporter" {
		principal = (hubscope.NativeScope{Credential: credential}).Actor().PrincipalID
		if principal == "" {
			return diagnosticsReport{}, hubapi.NativeNotFound()
		}
	}

	organization := tracker.OrganizationID(s.appOrganizationID())
	var snapshot diagnosticstore.Snapshot
	policies := []policystore.Revision{}
	read := func(r tenantstore.Reader) error {
		reader, ok := r.(diagnosticstore.Reader)
		if !ok {
			return errors.New("diagnostics reader unavailable")
		}
		projectReader, ok := r.(accessstore.ProjectAccessPageReader)
		if !ok {
			return hubapi.ErrProjectParticipants
		}
		projects, err := projectReader.ProjectAccessPage(ctx, projectAccessFilter(credential, requested, "", 100))
		if err != nil {
			return err
		}
		if len(projects) > 100 {
			projectPartial = true
			projects = projects[:100]
		}
		projects, err = s.grantedAccessProjects(ctx, r, organization, credential, projects)
		if err != nil {
			return err
		}
		for _, project := range projects {
			ids = append(ids, project.ProjectID)
		}
		if requested != "" && (len(ids) != 1 || ids[0] != requested) {
			return operatortool.ErrAccessDenied
		}
		snapshot, err = reader.DiagnosticsSnapshot(ctx, ids, w.From, w.To, s.config.Clock().UTC(), principal)
		if err != nil {
			return err
		}
		for _, project := range ids[:min(len(ids), diagnosticstore.ObjectLimit)] {
			policy, err := r.ProjectPolicy(ctx, project)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			policies = append(policies, policy)
		}
		if len(ids) > diagnosticstore.ObjectLimit {
			snapshot.Partial = true
		}

		return nil
	}
	if repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository); ok {
		err = repository.ReadTenantSnapshot(ctx, organization, corestore.Serving, read)
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, read)
	}
	if err != nil {
		return diagnosticsReport{}, err
	}
	snapshot.Partial = snapshot.Partial || projectPartial
	if projectPartial {
		snapshot.ResidencePartial = true
	}
	report := projectTenantDiagnostics(snapshot, w)
	content := eventstore.NewContent(s.runDetailStore())

	attemptValues, attemptErrors := readDiagnosticObjects(snapshot.AttemptRecords, func(attempt diagnosticstore.Attempt) (tracker.NativeRunData, error) {
		if attempt.Event.ID == "" {
			return tracker.NativeRunData{}, artifact.ErrMissing
		}
		value, err := content.Event(ctx, attempt.Event)
		if err != nil {
			return tracker.NativeRunData{}, err
		}
		if value.Data.Run == nil || value.Data.Run.AttemptID != attempt.ID || value.AggregateID != attempt.WorkItemID || value.ProjectID != attempt.ProjectID {
			return tracker.NativeRunData{}, artifact.ErrIntegrity
		}
		return *value.Data.Run, nil
	})
	valuesByAttempt := make(map[string]tracker.NativeRunData, len(attemptValues))
	for index, value := range attemptValues {
		if err := attemptErrors[index]; errors.Is(err, fs.ErrNotExist) || errors.Is(err, artifact.ErrMissing) {
			report.Partial = true
			continue
		} else if err != nil {
			return diagnosticsReport{}, fmt.Errorf("read diagnostic attempt: %w", err)
		}
		valuesByAttempt[snapshot.AttemptRecords[index].ID] = value
	}
	planning, validation := populateTenantAttemptDiagnostics(&report, snapshot, valuesByAttempt, w)
	failures := diagnosticNativeFailures(snapshot, valuesByAttempt, w)
	failurePage := nativeFailureSignaturePage(groupNativeFailures(failures), 0, 100)
	report.Partial = report.Partial || failurePage.NextOffset != nil
	refusalCount := 0
	for _, failure := range failurePage.Items {
		if reason, ok := strings.CutPrefix(failure.Signature, "landing refused: "); ok {
			report.Refused = append(report.Refused, diagnosticsRefusal{Reason: reason, Count: failure.Count})
			refusalCount += failure.Count
		}
	}
	slices.SortFunc(report.Refused, func(a, b diagnosticsRefusal) int { return strings.Compare(a.Reason, b.Reason) })
	for index := range report.Coverage {
		if report.Coverage[index].Source == "Landing refusal reason" && refusalCount > 0 {
			report.Coverage[index].Observed = &refusalCount
		}
	}

	ready, claimed, skipped := 0, 0, 0
	for _, lease := range snapshot.Leases {
		if !lease.AcquiredAt.Before(w.From) {
			claimed++
		}
	}
	missingDecision := snapshot.SchedulerPartial
	skips := map[string]nativeAnalyticsSkip{}
	decisions := []eventstore.Event{}
	for _, event := range snapshot.Events {
		if event.Type == "scheduler.decision" && event.RecordedAt.Before(w.To) {
			decisions = append(decisions, event)
		}
	}
	readDecision := func(event eventstore.Event) (tracker.CollaborationEvent, error) { return content.Event(ctx, event) }
	decisionValues, decisionErrors := readDiagnosticObjects(decisions, readDecision)
	decisionPayloads := make(map[string]tracker.CollaborationEvent, len(decisionValues))
	for index, value := range decisionValues {
		if decisionErrors[index] == nil {
			decisionPayloads[decisions[index].ID] = value
		}
	}
	priorObjects := []eventstore.Event{}
	queuedPriors := map[string]bool{}
	for _, event := range decisions {
		value, ok := decisionPayloads[event.ID]
		if !ok || value.Data.Decision == nil || value.Data.Decision.Source != "native_claim" || value.Data.Decision.Outcome != "claimed" {
			continue
		}
		prior, found := snapshot.PriorDecisions[event.ID]
		if _, read := decisionPayloads[prior.ID]; !found || read || queuedPriors[prior.ID] {
			continue
		}
		queuedPriors[prior.ID] = true
		priorObjects = append(priorObjects, prior)
	}
	priorValues, priorErrors := readDiagnosticObjects(priorObjects, readDecision)
	for index, value := range priorValues {
		if priorErrors[index] == nil {
			decisionPayloads[priorObjects[index].ID] = value
		}
	}
	for _, err := range append(slices.Clone(decisionErrors), priorErrors...) {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, artifact.ErrMissing) {
			report.Partial = true
		} else if err != nil {
			return diagnosticsReport{}, fmt.Errorf("read diagnostic scheduler event: %w", err)
		}
	}
	for index, value := range decisionValues {
		err := decisionErrors[index]
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, artifact.ErrMissing) {
			report.Partial = true
			missingDecision = true
			continue
		}
		if err != nil {
			return diagnosticsReport{}, fmt.Errorf("read diagnostic scheduler event: %w", err)
		}
		if decision := value.Data.Decision; decision != nil {
			switch decision.Outcome {
			case "ready":
				ready++
			case "skipped":
				skipped++
				key := decision.Source + "\x00" + decision.Reason
				skip := skips[key]
				skip.Source = decision.Source
				skip.Reason = decision.Reason
				skip.Count++
				skips[key] = skip
			}
		}
	}
	queue, queueClaims := 0, 0
	for _, event := range decisions {
		value, ok := decisionPayloads[event.ID]
		if !ok || value.Data.Decision == nil || value.Data.Decision.Source != "native_claim" || value.Data.Decision.Outcome != "claimed" {
			continue
		}
		queueClaims++
		prior, found := snapshot.PriorDecisions[event.ID]
		previous, decoded := decisionPayloads[prior.ID]
		if !found || !decoded {
			report.Partial = true
			continue
		}
		if nativeAnalyticsCapacityInterval(previous.Data.Decision, value.Data.Decision, prior.RecordedAt, event.RecordedAt) {
			queue++
		}
	}
	for index := range report.Coverage {
		if report.Coverage[index].Source == "Queue intervals" {
			report.Coverage[index].Observed = &queue
			report.Coverage[index].Total = &queueClaims
		}
	}
	report.Claimed = &claimed

	recordedDecisions := len(decisions) > 0 && !missingDecision
	if recordedDecisions || ready > 0 {
		report.Ready = &ready
	}
	if recordedDecisions || skipped > 0 {
		report.Skipped = &skipped
	}
	for index := range report.Coverage {
		if report.Coverage[index].Source == "Skip reasons" {
			report.Coverage[index].Observed = report.Skipped
			report.Coverage[index].Total = report.Skipped
		}
	}
	for _, skip := range skips {
		report.SkipReasons = append(report.SkipReasons, skip)
	}
	slices.SortFunc(report.SkipReasons, func(a, b nativeAnalyticsSkip) int {
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	values := map[string]map[string]bool{}

	policyValues, policyErrors := readDiagnosticObjects(policies, func(row policystore.Revision) (policy.Descriptor, error) { return content.ProjectPolicy(ctx, row) })
	for index, descriptor := range policyValues {
		err := policyErrors[index]
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, artifact.ErrMissing) {
			report.Partial = true
			continue
		}
		if err != nil {
			return diagnosticsReport{}, err
		}
		for name, value := range map[string]string{"Plan gate": strconv.FormatBool(descriptor.Gates.PlanEnabled), "Validator gate": strconv.FormatBool(descriptor.Gates.Validator), "Gate kind": descriptor.Gates.Kind, "Merge method": descriptor.Gates.MergeMethod} {
			if values[name] == nil {
				values[name] = map[string]bool{}
			}
			values[name][value] = true
		}
	}

	for _, name := range []string{"Plan gate", "Validator gate", "Gate kind", "Merge method"} {
		row := diagnosticsConfiguration{Name: name}
		if len(values[name]) > 1 {
			value := "Varies by project"
			row.Value = &value
		} else {
			for value := range values[name] {
				if value != "" {
					row.Value = &value
				}
			}
		}
		if name == "Plan gate" && planning > 0 {
			effect := fmt.Sprintf("%d recorded planning phases", planning)
			row.Effect = &effect
		}
		if name == "Validator gate" && validation > 0 {
			effect := fmt.Sprintf("%d recorded validation phases", validation)
			row.Effect = &effect
		}
		if name == "Merge method" && report.MergeLanded > 0 {
			effect := fmt.Sprintf("%d recorded landings", report.MergeLanded)
			row.Effect = &effect
		}
		report.Configuration = append(report.Configuration, row)
	}
	report.Requests, err = s.diagnosticsRequestMetrics(ctx)
	return report, err
}
