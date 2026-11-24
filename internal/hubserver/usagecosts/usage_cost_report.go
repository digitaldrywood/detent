package usagecosts

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

type Total struct {
	Bucket          string `json:"bucket"`
	Currency        string `json:"currency"`
	KnownMicros     *int64 `json:"known_micros"`
	EstimatedMicros *int64 `json:"estimated_micros"`
	ReportedMicros  *int64 `json:"reported_micros"`
	BilledMicros    *int64 `json:"billed_micros"`
	Unknown         int    `json:"unknown_observations"`
	UnreportedCosts int    `json:"unreported_cost_observations"`
	Stale           int    `json:"stale_observations"`
}

type ProjectCost struct {
	ProjectID string  `json:"project_id"`
	Totals    []Total `json:"totals"`
}

type Source struct {
	AttributedObservation
	AllocatedQuantity *float64 `json:"allocated_quantity"`
	AllocatedMicros   *int64   `json:"allocated_micros"`
	AllocatedBasis    string   `json:"allocated_basis"`
	Allocation        string   `json:"allocation"`
	Stale             bool     `json:"stale"`
}

type Resource struct {
	ProjectID       string `json:"project_id"`
	Provider        string `json:"provider"`
	ProviderAccount string `json:"provider_account"`
	ResourceID      string `json:"resource_id,omitempty"`
	ResourceName    string `json:"resource_name,omitempty"`
}

type CoverageGap struct {
	Resource
	Bucket string    `json:"bucket"`
	Metric string    `json:"metric"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
}

type Report struct {
	RunnerReporting    string        `json:"runner_reporting"`
	ActiveAttempts     int64         `json:"active_attempts"`
	UnreportedAttempts int64         `json:"unreported_attempts"`
	ActiveWorkComplete bool          `json:"active_work_complete"`
	OrganizationID     string        `json:"organization_id"`
	Scope              string        `json:"scope"`
	Timezone           string        `json:"timezone"`
	Period             Window        `json:"period"`
	GeneratedAt        time.Time     `json:"generated_at"`
	Coverage           string        `json:"coverage"`
	Totals             []Total       `json:"totals"`
	ByProject          []ProjectCost `json:"by_project"`
	Sources            []Source      `json:"sources"`
	Gaps               []CoverageGap `json:"gaps"`
	SourceCount        int           `json:"source_count"`
	GapCount           int           `json:"gap_count"`
	DetailsComplete    bool          `json:"details_complete"`
}

func addCostAmount(target **int64, value int64) error {
	if *target == nil {
		*target = new(int64)
	}
	previous := **target
	if value > 0 && previous > math.MaxInt64-value || value < 0 && previous < math.MinInt64-value {
		return errors.New("monthly cost total exceeds integer precision")
	}
	**target += value
	return nil
}

func addMonthlyCost(totals *[]Total, source Source) error {
	if source.Bucket == RunnerBucket && source.BillingMode == "subscription" {
		return nil
	}
	index := slices.IndexFunc(*totals, func(total Total) bool {
		return total.Bucket == source.Bucket && total.Currency == source.Currency
	})
	if index < 0 {
		*totals = append(*totals, Total{Bucket: source.Bucket, Currency: source.Currency})
		index = len(*totals) - 1
	}
	total := &(*totals)[index]
	if source.Bucket == RunnerBucket && (source.BillingMode != "metered" || source.ReportedAmountMicros == nil || source.Coverage != "complete") {
		total.UnreportedCosts++
	}
	if source.Stale {
		total.Stale++
	}
	if source.AllocatedMicros == nil {
		total.Unknown++
		return nil
	}
	if err := addCostAmount(&total.KnownMicros, *source.AllocatedMicros); err != nil {
		return err
	}
	if source.AllocatedBasis == "runner_reported" {
		return addCostAmount(&total.ReportedMicros, *source.AllocatedMicros)
	}
	if source.AllocatedBasis == "provider_billed" {
		return addCostAmount(&total.BilledMicros, *source.AllocatedMicros)
	}
	return addCostAmount(&total.EstimatedMicros, *source.AllocatedMicros)
}

func BuildReport(organization, scope string, window Window, now time.Time, observations []AttributedObservation, resources []Resource) (Report, error) {
	report := Report{OrganizationID: organization, Scope: scope, Timezone: "UTC", Period: window, GeneratedAt: now.UTC(), Coverage: "unknown", Totals: []Total{}, ByProject: []ProjectCost{}, Sources: []Source{}, Gaps: []CoverageGap{}}
	type resourceMetric struct {
		resource Resource
		bucket   string
		metric   string
	}
	coverage := map[resourceMetric][]Window{}
	for _, entry := range observations {
		start, end := entry.From, entry.To
		if start.Before(window.From) {
			start = window.From
		}
		if end.After(window.To) {
			end = window.To
		}
		if !end.After(start) {
			continue
		}
		source := Source{AttributedObservation: entry, Stale: now.After(entry.FreshUntil) && entry.AttemptID == "" && entry.Bucket != LunaBucket, AllocatedBasis: entry.Basis, Allocation: "whole_period"}
		if !start.Equal(entry.From) || !end.Equal(entry.To) {
			source.Allocation = "duration_prorated"
			if source.AllocatedBasis == "provider_billed" || source.AllocatedBasis == "runner_reported" {
				source.AllocatedBasis = "estimated"
			}
		}
		if entry.AmountMicros != nil {
			value := allocateMicros(*entry.AmountMicros, entry.From, entry.To, start, end)
			source.AllocatedMicros = &value
		}
		if entry.Quantity != nil {
			value := *entry.Quantity * float64(end.Sub(start)) / float64(entry.To.Sub(entry.From))
			source.AllocatedQuantity = &value
		}
		if err := addMonthlyCost(&report.Totals, source); err != nil {
			return report, err
		}
		index := slices.IndexFunc(report.ByProject, func(p ProjectCost) bool { return p.ProjectID == entry.ProjectID })
		if index < 0 {
			report.ByProject = append(report.ByProject, ProjectCost{ProjectID: entry.ProjectID, Totals: []Total{}})
			index = len(report.ByProject) - 1
		}
		if err := addMonthlyCost(&report.ByProject[index].Totals, source); err != nil {
			return report, err
		}
		report.Sources = append(report.Sources, source)
		if entry.AttemptID != "" || entry.Bucket == LunaBucket {
			if entry.Coverage != "complete" || entry.Quantity == nil || entry.AmountMicros == nil && entry.BillingMode != "subscription" {
				report.Gaps = append(report.Gaps, CoverageGap{Resource: Resource{ProjectID: entry.ProjectID, Provider: entry.Provider, ProviderAccount: entry.ProviderAccount, ResourceID: entry.ResourceID}, Bucket: entry.Bucket, Metric: entry.Metric, From: start, To: end})
			}
			continue
		}
		resource := Resource{ProjectID: entry.ProjectID, Provider: entry.Provider, ProviderAccount: entry.ProviderAccount, ResourceID: entry.ResourceID, ResourceName: entry.ResourceName}
		if resource.ResourceID != "" {
			resource.ResourceName = ""
		}
		key := resourceMetric{resource: resource, bucket: entry.Bucket, metric: entry.Metric}
		if _, found := coverage[key]; !found {
			coverage[key] = nil
		}
		if entry.Bucket == SpriteBucket {
			for _, metric := range SpriteMetrics() {
				expected := resourceMetric{resource: resource, bucket: SpriteBucket, metric: metric}
				if _, found := coverage[expected]; !found {
					coverage[expected] = nil
				}
			}
		}
		if entry.Coverage == "complete" && !source.Stale && entry.Quantity != nil && entry.AmountMicros != nil {
			coverage[key] = append(coverage[key], Window{From: start, To: end})
		}
	}
	for _, resource := range resources {
		found := slices.ContainsFunc(observations, func(o AttributedObservation) bool {
			return o.Bucket == SpriteBucket && o.ProjectID == resource.ProjectID && o.Provider == resource.Provider && o.ProviderAccount == resource.ProviderAccount && (o.ResourceID == resource.ResourceID && resource.ResourceID != "" || o.ResourceName == resource.ResourceName && resource.ResourceName != "")
		})
		if !found {
			for _, metric := range SpriteMetrics() {
				coverage[resourceMetric{resource: resource, bucket: SpriteBucket, metric: metric}] = nil
			}
		}
	}
	for key, spans := range coverage {
		slices.SortFunc(spans, func(a, b Window) int { return a.From.Compare(b.From) })
		cursor := window.From
		for _, span := range spans {
			if span.From.After(cursor) {
				report.Gaps = append(report.Gaps, CoverageGap{Resource: key.resource, Bucket: key.bucket, Metric: key.metric, From: cursor, To: span.From})
			}
			if span.To.After(cursor) {
				cursor = span.To
			}
		}
		if cursor.Before(window.To) {
			report.Gaps = append(report.Gaps, CoverageGap{Resource: key.resource, Bucket: key.bucket, Metric: key.metric, From: cursor, To: window.To})
		}
	}
	if len(report.Sources) > 0 {
		report.Coverage = "partial"
		if len(report.Gaps) == 0 {
			report.Coverage = "complete"
		}
	}
	sortTotals := func(totals []Total) {
		slices.SortFunc(totals, func(a, b Total) int {
			return strings.Compare(a.Bucket+"\x00"+a.Currency, b.Bucket+"\x00"+b.Currency)
		})
	}
	sortTotals(report.Totals)
	slices.SortFunc(report.ByProject, func(a, b ProjectCost) int { return strings.Compare(a.ProjectID, b.ProjectID) })
	for _, project := range report.ByProject {
		sortTotals(project.Totals)
	}
	slices.SortFunc(report.Gaps, func(a, b CoverageGap) int {
		left := []string{a.ProjectID, a.Provider, a.ProviderAccount, a.ResourceID, a.ResourceName, a.Bucket, a.Metric, a.From.Format(TimeLayout)}
		right := []string{b.ProjectID, b.Provider, b.ProviderAccount, b.ResourceID, b.ResourceName, b.Bucket, b.Metric, b.From.Format(TimeLayout)}
		return slices.Compare(left, right)
	})
	report.SourceCount, report.GapCount, report.DetailsComplete = len(report.Sources), len(report.Gaps), true
	return report, nil
}

func BoundedDetails(report Report) Report {
	const limit = 32
	if len(report.Sources) > limit {
		report.Sources = report.Sources[:limit]
		report.DetailsComplete = false
	}
	if len(report.Gaps) > limit {
		report.Gaps = report.Gaps[:limit]
		report.DetailsComplete = false
	}
	return report
}

func (s *Service) Monthly(ctx context.Context, organization, scope string, projects []string, window Window, now time.Time) (Report, error) {
	observations, resources, snapshot, err := s.MonthlyRecords(ctx, organization, projects, window)
	if err != nil {
		return Report{}, err
	}
	report, err := BuildReport(organization, scope, window, now, observations, resources)
	if err != nil {
		return Report{}, err
	}
	report.RunnerReporting = "finish_time"
	report.ActiveAttempts, report.UnreportedAttempts = snapshot.ActiveAttempts, snapshot.UnreportedAttempts
	report.ActiveWorkComplete = report.ActiveAttempts == 0 && report.UnreportedAttempts == 0
	if !report.ActiveWorkComplete && report.Coverage == "complete" {
		report.Coverage = "partial"
	}
	return report, nil
}
