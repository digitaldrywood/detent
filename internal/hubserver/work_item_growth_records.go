package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type workItemGrowth struct {
	collaborationBytes int64
	openIssues         int64
	historyRecords     int64
	events             int64
}

func issueCollaborationFieldBytes(issue tracker.NativeIssue) (int64, error) {
	labels, err := json.Marshal(issue.Labels)
	if err != nil {
		return 0, err
	}
	assignees, err := json.Marshal(issue.Assignees)
	if err != nil {
		return 0, err
	}
	return int64(len(issue.Title) + len(issue.Body) + len(labels) + len(assignees)), nil
}

func collaborationEventChargeBytes(raw []byte) (int64, error) {
	var event struct {
		Type  string          `json:"type"`
		Actor json.RawMessage `json:"actor"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return 0, err
	}
	if strings.HasPrefix(event.Type, "run.") || event.Type == "scheduler.decision" {
		return 0, nil
	}
	return int64(len(event.Actor) + len(event.Data)), nil
}

func openWorkItemState(project projectstore.Record, archived bool, state string) bool {
	if archived {
		return false
	}
	for _, candidate := range project.States {
		if candidate.Name == state {
			return !candidate.Terminal
		}
	}
	return true
}

func (s *Service) applyWorkItemGrowth(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, receiptID string, growth workItemGrowth, completion bool) error {
	now := s.config.Clock()
	plans := s.database.plansService().Plans()
	var entitlement hostedplans.HostedEntitlement
	if plans != nil {
		var err error
		entitlement, err = hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, w, now))
		if err != nil {
			return err
		}
	}
	if delta := growth.collaborationBytes; delta != 0 {
		limit := int64(math.MaxInt64)
		if value, limited := entitlement.Allowances["collaboration_bytes"]; limited && !completion {
			limit = value
		}
		if delta < 0 {
			used, err := w.CollaborationBytes(ctx)
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.AttachmentAccountingUnavailable()
			}
			if err != nil {
				return err
			}
			delta = max(delta, -used)
		}
		usage, err := w.AdjustCollaborationBytes(ctx, delta, limit)
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
	if plans == nil {
		return nil
	}
	if populations, ok := w.(billingstore.PopulationReader); ok && !completion {
		for _, population := range []struct {
			name   string
			growth int64
		}{{"unarchived_issues", growth.openIssues}, {"history_records", growth.historyRecords}} {
			limit, limited := entitlement.Allowances[population.name]
			if !limited || population.growth <= 0 {
				continue
			}
			used, known, err := populations.BillingPopulation(ctx, population.name, now, time.Duration(plans.ConnectedSeconds)*time.Second)
			if err != nil {
				return err
			}
			if known && used+population.growth > limit {
				return &hostedplans.LimitError{Resource: population.name, Allowance: limit, Consumption: used}
			}
		}
	}
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	for _, sample := range []struct {
		metric string
		amount int64
	}{{"api_mutations", 1}, {"ingested_events", growth.events}} {
		if sample.amount <= 0 || sample.metric == "api_mutations" && ctx.Value(runnerRequestBudgetContext{}) == true {
			continue
		}
		limit := entitlement.Allowances[sample.metric]
		if completion {
			limit = math.MaxInt64
		}
		usage, err := w.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: organization, ID: receiptID + "/" + sample.metric, WindowStart: window, Metric: sample.metric, Amount: sample.amount}, limit)
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedplans.LimitError{Resource: sample.metric, Allowance: limit, Consumption: usage.Used}
		}
	}
	return nil
}

type runEventGrowth struct {
	enabled      bool
	completion   bool
	window       time.Time
	entitlement  hostedplans.HostedEntitlement
	history      int64
	historyKnown bool
}

func (s *Service) prepareRunEventGrowth(ctx context.Context, w tenantstore.Writer, completion bool) (runEventGrowth, error) {
	plans := s.database.plansService().Plans()
	if plans == nil {
		return runEventGrowth{}, nil
	}
	now := s.config.Clock()
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	if completion {
		return runEventGrowth{enabled: true, completion: true, window: window}, nil
	}
	entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, w, now))
	if err != nil {
		return runEventGrowth{}, err
	}
	growth := runEventGrowth{enabled: true, window: window, entitlement: entitlement}
	if _, limited := entitlement.Allowances["history_records"]; limited {
		if populations, ok := w.(billingstore.PopulationReader); ok {
			growth.history, growth.historyKnown, err = populations.BillingPopulation(ctx, "history_records", now, time.Duration(plans.ConnectedSeconds)*time.Second)
			if err != nil {
				return runEventGrowth{}, err
			}
		}
	}
	return growth, nil
}

func (g runEventGrowth) apply(ctx context.Context, w tenantstore.Writer, organization tracker.OrganizationID, receiptID string, events int64) error {
	if !g.enabled || events <= 0 {
		return nil
	}
	if limit, limited := g.entitlement.Allowances["history_records"]; limited && !g.completion && g.historyKnown && g.history+2*events > limit {
		return &hostedplans.LimitError{Resource: "history_records", Allowance: limit, Consumption: g.history}
	}
	limit := g.entitlement.Allowances["ingested_events"]
	if g.completion {
		limit = math.MaxInt64
	}
	usage, err := w.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: organization, ID: receiptID + "/ingested_events", WindowStart: g.window, Metric: "ingested_events", Amount: events}, limit)
	if err != nil {
		return err
	}
	if !usage.Allowed {
		return &hostedplans.LimitError{Resource: "ingested_events", Allowance: limit, Consumption: usage.Used}
	}
	return nil
}
