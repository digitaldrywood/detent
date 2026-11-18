package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Per-attempt usage ingestion. A run event carries
// the attempt's running total; the hub records only the increment over what
// it already holds for the attempt, filed under the hour the event arrived.
// A redelivered event therefore adds nothing, spend after midnight lands on
// the day it happened, and a runner that reports nothing leaves the stored
// total alone.

const (
	// maxNativeUsageEntries bounds one report: a turn uses one model, and an
	// attempt that touched more than this many is not a usage problem.
	maxNativeUsageEntries = 32
	// maxNativeUsageNameBytes bounds a provider or model identifier.
	maxNativeUsageNameBytes = 128
	usagePeriodLayout       = usagereport.PeriodLayout
)

// validateNativeRunUsage checks the usage a run event carries. Usage belongs
// to a checkpoint or a finish; a run that has only started has spent nothing
// worth reporting.
func validateNativeRunUsage(request tracker.NativeRunEvent) error {
	usage := request.Data.Usage
	if len(usage) == 0 {
		return nil
	}
	if request.Type == "run.started" {
		return hubapi.NativePublicInvalid("Usage requires a checkpoint or a finished run")
	}
	if len(usage) > maxNativeUsageEntries {
		return hubapi.NativeInvalid(fmt.Sprintf("A run event carries at most %d usage entries", maxNativeUsageEntries))
	}
	seen := make(map[string]bool, len(usage))
	for _, entry := range usage {
		provider := strings.TrimSpace(entry.Provider)
		model := strings.TrimSpace(entry.Model)
		if provider == "" || model == "" || len(provider) > maxNativeUsageNameBytes || len(model) > maxNativeUsageNameBytes {
			return hubapi.NativePublicInvalid("Usage requires a provider and a model of at most 128 bytes each")
		}
		key := tracker.UsageProviderID(provider) + "\x00" + model
		if entry.UsageKind == "incremental" {
			key += "\x00" + entry.SourceID
		}
		if err := validateRunnerCostUsage(entry); err != nil {
			return err
		}
		if seen[key] {
			return hubapi.NativeInvalid("Usage entries must name distinct provider and model pairs")
		}
		seen[key] = true
		if entry.Input < 0 || entry.CachedInput < 0 || entry.Output < 0 || entry.CostEstimate < 0 {
			return hubapi.NativePublicInvalid("Usage counts and cost cannot be negative")
		}
	}
	return nil
}

// usagePeriod is the hour bucket a moment is filed under.
func usagePeriod(at time.Time) string {
	return at.UTC().Truncate(time.Hour).Format(usagePeriodLayout)
}

func prepareAttemptUsage(scope hubscope.NativeScope, item tracker.NativeWorkItemID, event tracker.NativeRunEvent, prices UsageConfig, now time.Time) reportstore.AttemptUsage {
	report := reportstore.AttemptUsage{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, AttemptID: event.Data.AttemptID, LeaseID: string(event.Data.LeaseID), FencingToken: int64(event.Data.FencingToken), Sequence: event.Data.Sequence, ReceivedAt: now.UTC(), Currency: strings.ToUpper(strings.TrimSpace(prices.currency()))}
	for _, entry := range event.Data.Usage {
		if entry.UsageKind == "incremental" {
			continue
		}
		total := reportstore.AttemptUsageTotal{Provider: tracker.UsageProviderID(entry.Provider), Model: strings.TrimSpace(entry.Model), Input: entry.Input, CachedInput: entry.CachedInput, Output: entry.Output}
		currency := strings.ToUpper(strings.TrimSpace(entry.Currency))
		if entry.CostEstimate > 0 && (currency == "" || currency == report.Currency) {
			total.ReportedCost = new(entry.CostEstimate)
		} else if price, found := prices.price(entry.Model); found {
			total.InputPrice, total.CachedInputPrice, total.OutputPrice = price.Input, price.CachedInput, price.Output
		}
		report.Totals = append(report.Totals, total)
	}
	return report
}

func (s *Service) prepareAttemptUsageHistory(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, event tracker.NativeRunEvent, attempt leasestore.Attempt) ([]billingstore.CostObservation, error) {
	type selected struct {
		source   billingstore.CostSource
		revision billingstore.CostRevision
	}
	rows := make([]selected, 0, len(event.Data.Usage))
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		for _, entry := range event.Data.Usage {
			source := billingstore.CostSource{Provider: tracker.UsageProviderID(entry.Provider), ProviderAccount: "runner", SourceID: event.Data.AttemptID + ":" + strings.TrimSpace(entry.Model), From: entry.From, To: entry.To}
			if entry.UsageKind == "incremental" {
				source.SourceID += ":" + entry.SourceID
			}
			if source.From.IsZero() {
				source.From = attempt.StartedAt
			}
			if source.To.IsZero() {
				source.To = attempt.UpdatedAt
			}
			revision, err := r.BillingCostRevision(ctx, source)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if revision.ProjectID != scope.Project {
				return artifact.ErrIntegrity
			}
			rows = append(rows, selected{source: source, revision: revision})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	records := make([]billingstore.CostObservation, 0, len(rows))
	for _, row := range rows {
		ref := row.revision.Reference.ArchiveReference()
		stored, err := attachment.ReadVerifiedDetail(ctx, s.runDetailStore(), string(scope.Organization), ref)
		if err != nil {
			return nil, err
		}
		raw, _ := stored.Content(ref)
		var observation costObservation
		if json.Unmarshal(raw, &observation) != nil || observation.Provider != row.source.Provider || observation.ProviderAccount != row.source.ProviderAccount || observation.SourceID != row.source.SourceID || !observation.From.Equal(row.source.From) || !observation.To.Equal(row.source.To) || observation.Revision != row.revision.Revision || observation.ResourceID != event.Data.AttemptID || observation.AttemptID != event.Data.AttemptID || observation.WorkItemID != string(item) || observation.Bucket != runnerCostBucket {
			return nil, artifact.ErrIntegrity
		}
		records = append(records, billingstore.CostObservation{OrganizationID: scope.Organization, ProjectID: scope.Project, Observation: observation, Reference: row.revision.Reference, ReceivedAt: row.revision.ReceivedAt, Stored: stored})
	}
	return records, nil
}

type nativeUsageEventEnvelope struct {
	tracker.CollaborationEvent
	Usage *reportstore.AttemptUsage `json:"_hub_usage_v1,omitempty"`
}

type nativeUsageEnvelopeStore struct {
	attachment.DetailStore
	payload []byte
}

func (s *nativeUsageEnvelopeStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	value, err := s.DetailStore.ReadDetail(ctx, organization, ref)
	if err == nil {
		s.payload = value
	}
	return value, err
}

const usageRefreshRows = 100

func (s *Service) UsageMaintenanceFeature() hubmaintenance.Feature {
	type cursor struct {
		mu           sync.Mutex
		generation   int64
		sourcesAfter string
		sourceTurn   bool
		afterProject tracker.ProjectID
		afterAttempt string
	}
	var mu sync.Mutex
	cursors := map[string]*cursor{}
	return hubmaintenance.Feature{Name: "usage", Bytes: hubmaintenance.ByteLimit, Step: func(ctx context.Context, selected hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		if err := hubmaintenance.RequireTenant(ctx, selected); err != nil {
			return hubmaintenance.Outcome{}, err
		}
		mu.Lock()
		current := cursors[selected.Organization]
		if current == nil {
			current = new(cursor)
			cursors[selected.Organization] = current
		}
		mu.Unlock()
		current.mu.Lock()
		defer current.mu.Unlock()
		if current.generation != selected.Generation {
			current.generation = selected.Generation
			current.sourcesAfter = ""
			current.sourceTurn = false
			current.afterProject = ""
			current.afterAttempt = ""
		}
		organization := tracker.OrganizationID(selected.Organization)
		refresh := func() (hubmaintenance.Outcome, error) {
			owner, ok := s.config.TenantRepository.(interface {
				RefreshReportUsagePage(context.Context, tracker.OrganizationID, attachment.DetailStore, string, int) (string, int, error)
			})
			if !ok {
				return hubmaintenance.Outcome{}, hubapi.NativeRunEventStorageUnavailable()
			}
			next, count, err := owner.RefreshReportUsagePage(ctx, organization, hubmaintenance.LimitDetails(s.runDetailStore()), current.sourcesAfter, usageRefreshRows)
			if err == nil {
				current.sourcesAfter = next
				if count == 0 {
					current.sourcesAfter = ""
				}
			}
			return hubmaintenance.Outcome{Progress: count > 0, Items: 1}, err
		}
		if current.sourceTurn {
			current.sourceTurn = false
			return refresh()
		}
		current.sourceTurn = true
		read := func(afterProject tracker.ProjectID, afterAttempt string) (reportstore.PendingUsageAttempt, error) {
			var pending reportstore.PendingUsageAttempt
			err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
				owner, ok := r.(reportstore.UsageReplayReader)
				if !ok {
					return hubapi.NativeRunEventStorageUnavailable()
				}
				var err error
				pending, err = owner.NextPendingUsageAttempt(ctx, afterProject, afterAttempt)
				return err
			})
			return pending, err
		}
		idle := hubmaintenance.Outcome{Items: 1}
		pending, err := read(current.afterProject, current.afterAttempt)
		if errors.Is(err, corestore.ErrNotFound) && (current.afterProject != "" || current.afterAttempt != "") {
			pending, err = read("", "")
		}
		if errors.Is(err, corestore.ErrNotFound) {
			current.afterProject, current.afterAttempt = "", ""
			current.sourceTurn = false
			return refresh()
		}
		if err != nil {
			return idle, err
		}
		current.afterProject, current.afterAttempt = pending.ProjectID, pending.AttemptID
		owner, ok := s.config.TenantRepository.(interface {
			RefreshAttemptReportUsage(context.Context, tracker.OrganizationID, string, int) (bool, error)
		})
		if !ok {
			return idle, hubapi.NativeRunEventStorageUnavailable()
		}
		return hubmaintenance.RefreshThenReplay(budget.Items,
			func() (bool, error) {
				return owner.RefreshAttemptReportUsage(ctx, organization, pending.AttemptID, usageRefreshRows)
			},
			func(limit int) ([]eventstore.Event, error) {
				var events []eventstore.Event
				err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
					reader, ok := r.(eventstore.AttemptEventPageReader)
					if !ok {
						return hubapi.NativeRunEventStorageUnavailable()
					}
					var err error
					events, err = reader.AttemptEvents(ctx, pending.ProjectID, pending.WorkItemID, pending.AttemptID, pending.AfterEventSequence, limit)
					return err
				})
				return events, err
			},
			func(event eventstore.Event) (bool, error) { return s.replayNativeUsageEvent(ctx, event) })
	}}
}

func (s *Service) replayNativeUsageEvent(ctx context.Context, event eventstore.Event) (bool, error) {
	store := &nativeUsageEnvelopeStore{DetailStore: hubmaintenance.LimitDetails(s.runDetailStore())}
	value, err := eventstore.NewContent(store).Event(ctx, event)
	if err != nil {
		return false, err
	}
	var envelope nativeUsageEventEnvelope
	if json.Unmarshal(store.payload, &envelope) != nil || value.Data.Run == nil {
		return false, artifact.ErrIntegrity
	}
	run := value.Data.Run
	report := reportstore.AttemptUsage{OrganizationID: event.OrganizationID, ProjectID: event.ProjectID, WorkItemID: event.WorkItemID, AttemptID: run.AttemptID, LeaseID: string(run.LeaseID), FencingToken: int64(run.FencingToken), Sequence: run.Sequence, ReceivedAt: event.RecordedAt, PricingUnavailable: len(run.Usage) > 0}
	if envelope.Usage != nil {
		report = *envelope.Usage
		if report.OrganizationID != event.OrganizationID || report.ProjectID != event.ProjectID || report.WorkItemID != event.WorkItemID || report.AttemptID != run.AttemptID || report.LeaseID != string(run.LeaseID) || report.FencingToken != int64(run.FencingToken) || report.Sequence != run.Sequence || !report.ReceivedAt.UTC().Truncate(time.Microsecond).Equal(event.RecordedAt) || len(report.Historical) != 0 || report.EventSequence != 0 || report.PricingUnavailable {
			return false, artifact.ErrIntegrity
		}
	}
	report.ReceivedAt = event.RecordedAt
	report.ReplayEventID, report.ReplayEventSHA256, report.EventSequence = event.ID, event.PayloadSHA256, event.Sequence
	var complete bool
	err = s.config.TenantRepository.WriteTenant(ctx, event.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(reportstore.UsageReplayWriter)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		complete, err = owner.ReplayAttemptUsage(ctx, reportstore.UsageReplay{Usage: report, Projection: *reportstore.ProjectEvent(value)})
		return err
	})
	return complete, err
}
