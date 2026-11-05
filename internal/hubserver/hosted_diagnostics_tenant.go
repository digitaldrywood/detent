package hubserver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"

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

func (s *Service) readCloudDiagnosticsTenant(ctx context.Context, credential apiCredential, w operatortool.AnalyticsWindow) (diagnosticsReport, error) {
	var ids []tracker.ProjectID
	var err error
	projectPartial := false
	principal := ""
	if credential.HostedRole == "reporter" {
		principal = (nativeScope{credential: credential}).actor().PrincipalID
		if principal == "" {
			return diagnosticsReport{}, nativeNotFound()
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
			return ErrProjectParticipants
		}
		projects, err := projectReader.ProjectAccessPage(ctx, projectAccessFilter(credential, "", "", 100))
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
		snapshot, err = reader.DiagnosticsSnapshot(ctx, ids, w.From, w.To, s.config.now().UTC(), principal)
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
	decisionValues, decisionErrors := readDiagnosticObjects(decisions, func(event eventstore.Event) (tracker.CollaborationEvent, error) { return content.Event(ctx, event) })
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
	report.Claimed = &claimed

	if !missingDecision || ready > 0 {
		report.Ready = &ready
	}
	if !missingDecision || skipped > 0 {
		report.Skipped = &skipped
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
		report.Configuration = append(report.Configuration, row)
	}
	report.Requests, err = s.diagnosticsRequestMetrics(ctx)
	return report, err
}

func projectTenantDiagnostics(snapshot diagnosticstore.Snapshot, w operatortool.AnalyticsWindow) diagnosticsReport {
	out := diagnosticsReport{From: w.From, To: w.To, Partial: snapshot.Partial, Capacity: []diagnosticsCapacity{}, SkipReasons: []nativeAnalyticsSkip{}, StorageConversions: []diagnosticsStorageConversion{}, Refused: []diagnosticsRefusal{}}
	transitions := 0
	waits := []float64{}
	mergeStarted := map[[2]string]time.Time{}
	for _, event := range snapshot.Events {
		if event.Type != "workflow.transitioned" || !event.RecordedAt.Before(w.To) {
			continue
		}
		transitions++
		key := [2]string{string(event.ProjectID), string(event.WorkItemID)}
		if event.ToState == "Merging" {
			out.MergeEntered++
			mergeStarted[key] = event.RecordedAt
		}
		if event.FromState == "Merging" {
			if start, ok := mergeStarted[key]; ok {
				waits = append(waits, event.RecordedAt.Sub(start).Seconds())
				delete(mergeStarted, key)
			}
			if event.ToState == "Done" {
				out.MergeLanded++
			}
		}
	}
	for at := w.From; at.Before(w.To); at = at.Add(time.Hour) {
		out.Capacity = append(out.Capacity, diagnosticsCapacity{Hour: at})
	}
	populateTenantTodo(&out, snapshot)
	populateTenantLeaseOccupancy(&out, snapshot.Leases)

	if len(waits) > 0 {
		duration := analyticsDuration(waits)
		out.MergeWaitP50 = &duration.P50Seconds
		out.MergeWaitP90 = &duration.P90Seconds
		maximum := slices.Max(waits)
		out.MergeWaitMax = &maximum
	}
	out.Coverage = []diagnosticsCoverage{{Source: "Transitions", Observed: &transitions}, {Source: "Attempts", Observed: &snapshot.Attempts}, {Source: "Usage joins", Total: &snapshot.Attempts}, {Source: "Activity receipts"}, {Source: "Queue intervals"}, {Source: "Merge identity", Total: &out.MergeLanded}, {Source: "Landing refusal reason"}, {Source: "Detector tick"}}
	return out
}

func readDiagnosticObjects[T, R any](records []T, read func(T) (R, error)) ([]R, []error) {
	values := make([]R, len(records))
	errs := make([]error, len(records))
	jobs := make(chan int, len(records))
	for index := range records {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(4, len(records)) {
		workers.Go(func() {
			for index := range jobs {
				values[index], errs[index] = read(records[index])
			}
		})
	}
	workers.Wait()
	return values, errs
}

func populateTenantTodo(out *diagnosticsReport, snapshot diagnosticstore.Snapshot) {
	if snapshot.ResidencePartial {
		return
	}
	states := make(map[[2]string]string, len(snapshot.Issues))
	todo := 0
	issues := slices.Clone(snapshot.Issues)
	slices.SortFunc(issues, func(a, b diagnosticstore.Issue) int { return b.CreatedAt.Compare(a.CreatedAt) })
	for _, issue := range issues {
		states[[2]string{string(issue.ProjectID), string(issue.WorkItemID)}] = issue.State
		if issue.State == "Todo" {
			todo++
		}
	}
	eventIndex, issueIndex := len(snapshot.Events)-1, 0
	for index := len(out.Capacity) - 1; index >= 0; index-- {
		end := minTime(out.Capacity[index].Hour.Add(time.Hour), out.To)
		for eventIndex >= 0 && !snapshot.Events[eventIndex].RecordedAt.Before(end) {
			event := snapshot.Events[eventIndex]
			eventIndex--
			if event.Type != "workflow.transitioned" {
				continue
			}
			key := [2]string{string(event.ProjectID), string(event.WorkItemID)}
			state, ok := states[key]
			if !ok {
				continue
			}
			if state == "Todo" {
				todo--
			}
			states[key] = event.FromState
			if event.FromState == "Todo" {
				todo++
			}
		}
		for issueIndex < len(issues) && issues[issueIndex].CreatedAt.After(end) {
			issue := issues[issueIndex]
			issueIndex++
			key := [2]string{string(issue.ProjectID), string(issue.WorkItemID)}
			if states[key] == "Todo" {
				todo--
			}
			delete(states, key)
		}
		depth := todo
		out.Capacity[index].Todo = &depth
	}
}

func populateTenantLeaseOccupancy(out *diagnosticsReport, leases []diagnosticstore.Lease) {
	if len(out.Capacity) == 0 {
		return
	}
	held := make([]float64, len(out.Capacity))
	delta := make([]float64, len(out.Capacity)+1)
	for _, lease := range leases {
		from, to := maxTime(lease.AcquiredAt, out.From), minTime(lease.FinishedAt, out.To)
		if !from.Before(to) {
			continue
		}
		first := int(from.Sub(out.From) / time.Hour)
		last := int(to.Add(-time.Nanosecond).Sub(out.From) / time.Hour)
		if first == last {
			held[first] += to.Sub(from).Seconds()
			continue
		}
		held[first] += out.Capacity[first].Hour.Add(time.Hour).Sub(from).Seconds()
		held[last] += to.Sub(out.Capacity[last].Hour).Seconds()
		if last > first+1 {
			delta[first+1] += time.Hour.Seconds()
			delta[last] -= time.Hour.Seconds()
		}
	}
	active := 0.0
	for index := range out.Capacity {
		active += delta[index]
		bucket := &out.Capacity[index]
		bucket.Slots = (held[index] + active) / minTime(bucket.Hour.Add(time.Hour), out.To).Sub(bucket.Hour).Seconds()
	}
}
