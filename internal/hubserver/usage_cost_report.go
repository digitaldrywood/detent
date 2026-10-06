package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

type monthlyCostTotal struct {
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

type monthlyProjectCost struct {
	ProjectID string             `json:"project_id"`
	Totals    []monthlyCostTotal `json:"totals"`
}

type monthlyCostSource struct {
	attributedCostObservation
	AllocatedQuantity *float64 `json:"allocated_quantity"`
	AllocatedMicros   *int64   `json:"allocated_micros"`
	AllocatedBasis    string   `json:"allocated_basis"`
	Allocation        string   `json:"allocation"`
	Stale             bool     `json:"stale"`
}

type costResource struct {
	ProjectID       string `json:"project_id"`
	Provider        string `json:"provider"`
	ProviderAccount string `json:"provider_account"`
	ResourceID      string `json:"resource_id,omitempty"`
	ResourceName    string `json:"resource_name,omitempty"`
}

type costCoverageGap struct {
	costResource
	Bucket string    `json:"bucket"`
	Metric string    `json:"metric"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
}

type monthlyCostReport struct {
	RunnerReporting    string               `json:"runner_reporting"`
	ActiveAttempts     int64                `json:"active_attempts"`
	UnreportedAttempts int64                `json:"unreported_attempts"`
	ActiveWorkComplete bool                 `json:"active_work_complete"`
	OrganizationID     string               `json:"organization_id"`
	Scope              string               `json:"scope"`
	Timezone           string               `json:"timezone"`
	Period             usageWindow          `json:"period"`
	GeneratedAt        time.Time            `json:"generated_at"`
	Coverage           string               `json:"coverage"`
	Totals             []monthlyCostTotal   `json:"totals"`
	ByProject          []monthlyProjectCost `json:"by_project"`
	Sources            []monthlyCostSource  `json:"sources"`
	Gaps               []costCoverageGap    `json:"gaps"`
	SourceCount        int                  `json:"source_count"`
	GapCount           int                  `json:"gap_count"`
	DetailsComplete    bool                 `json:"details_complete"`
}

func addCostAmount(target **int64, value int64) error {
	if *target == nil {
		*target = new(int64)
	}
	previous := **target
	if value > 0 && previous > math.MaxInt64-value || value < 0 && previous < math.MinInt64-value {
		return fmt.Errorf("monthly cost total exceeds integer precision")
	}
	**target += value
	return nil
}

func addMonthlyCost(totals *[]monthlyCostTotal, source monthlyCostSource) error {
	if source.Bucket == runnerCostBucket && source.BillingMode == "subscription" {
		return nil
	}
	index := slices.IndexFunc(*totals, func(total monthlyCostTotal) bool {
		return total.Bucket == source.Bucket && total.Currency == source.Currency
	})
	if index < 0 {
		*totals = append(*totals, monthlyCostTotal{Bucket: source.Bucket, Currency: source.Currency})
		index = len(*totals) - 1
	}
	total := &(*totals)[index]
	if source.Bucket == runnerCostBucket && (source.BillingMode != "metered" || source.ReportedAmountMicros == nil || source.Coverage != "complete") {
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

func buildMonthlyCostReport(organization, scope string, window usageWindow, now time.Time, observations []attributedCostObservation, resources []costResource) (monthlyCostReport, error) {
	report := monthlyCostReport{OrganizationID: organization, Scope: scope, Timezone: "UTC", Period: window, GeneratedAt: now.UTC(), Coverage: "unknown", Totals: []monthlyCostTotal{}, ByProject: []monthlyProjectCost{}, Sources: []monthlyCostSource{}, Gaps: []costCoverageGap{}}
	type resourceMetric struct {
		resource costResource
		bucket   string
		metric   string
	}
	coverage := map[resourceMetric][]usageWindow{}
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
		source := monthlyCostSource{attributedCostObservation: entry, Stale: now.After(entry.FreshUntil) && entry.AttemptID == "" && entry.Bucket != lunaCostBucket, AllocatedBasis: entry.Basis, Allocation: "whole_period"}
		if !start.Equal(entry.From) || !end.Equal(entry.To) {
			source.Allocation = "duration_prorated"
			if source.AllocatedBasis == "provider_billed" || source.AllocatedBasis == "runner_reported" {
				source.AllocatedBasis = "estimated"
			}
		}
		if entry.AmountMicros != nil {
			value := allocateCostMicros(*entry.AmountMicros, entry.From, entry.To, start, end)
			source.AllocatedMicros = &value
		}
		if entry.Quantity != nil {
			value := *entry.Quantity * float64(end.Sub(start)) / float64(entry.To.Sub(entry.From))
			source.AllocatedQuantity = &value
		}
		if err := addMonthlyCost(&report.Totals, source); err != nil {
			return report, err
		}
		index := slices.IndexFunc(report.ByProject, func(p monthlyProjectCost) bool { return p.ProjectID == entry.ProjectID })
		if index < 0 {
			report.ByProject = append(report.ByProject, monthlyProjectCost{ProjectID: entry.ProjectID, Totals: []monthlyCostTotal{}})
			index = len(report.ByProject) - 1
		}
		if err := addMonthlyCost(&report.ByProject[index].Totals, source); err != nil {
			return report, err
		}
		report.Sources = append(report.Sources, source)
		if entry.AttemptID != "" || entry.Bucket == lunaCostBucket {
			if entry.Coverage != "complete" || entry.Quantity == nil || entry.AmountMicros == nil && entry.BillingMode != "subscription" {
				report.Gaps = append(report.Gaps, costCoverageGap{costResource: costResource{ProjectID: entry.ProjectID, Provider: entry.Provider, ProviderAccount: entry.ProviderAccount, ResourceID: entry.ResourceID}, Bucket: entry.Bucket, Metric: entry.Metric, From: start, To: end})
			}
			continue
		}
		resource := costResource{ProjectID: entry.ProjectID, Provider: entry.Provider, ProviderAccount: entry.ProviderAccount, ResourceID: entry.ResourceID, ResourceName: entry.ResourceName}
		if resource.ResourceID != "" {
			resource.ResourceName = ""
		}
		key := resourceMetric{resource: resource, bucket: entry.Bucket, metric: entry.Metric}
		if _, found := coverage[key]; !found {
			coverage[key] = nil
		}
		if entry.Bucket == spriteCostBucket {
			for _, metric := range spriteCostMetrics() {
				expected := resourceMetric{resource: resource, bucket: spriteCostBucket, metric: metric}
				if _, found := coverage[expected]; !found {
					coverage[expected] = nil
				}
			}
		}
		if entry.Coverage == "complete" && !source.Stale && entry.Quantity != nil && entry.AmountMicros != nil {
			coverage[key] = append(coverage[key], usageWindow{From: start, To: end})
		}
	}
	for _, resource := range resources {
		found := slices.ContainsFunc(observations, func(o attributedCostObservation) bool {
			return o.Bucket == spriteCostBucket && o.ProjectID == resource.ProjectID && o.Provider == resource.Provider && o.ProviderAccount == resource.ProviderAccount && (o.ResourceID == resource.ResourceID && resource.ResourceID != "" || o.ResourceName == resource.ResourceName && resource.ResourceName != "")
		})
		if !found {
			for _, metric := range spriteCostMetrics() {
				coverage[resourceMetric{resource: resource, bucket: spriteCostBucket, metric: metric}] = nil
			}
		}
	}
	for key, spans := range coverage {
		slices.SortFunc(spans, func(a, b usageWindow) int { return a.From.Compare(b.From) })
		cursor := window.From
		for _, span := range spans {
			if span.From.After(cursor) {
				report.Gaps = append(report.Gaps, costCoverageGap{costResource: key.resource, Bucket: key.bucket, Metric: key.metric, From: cursor, To: span.From})
			}
			if span.To.After(cursor) {
				cursor = span.To
			}
		}
		if cursor.Before(window.To) {
			report.Gaps = append(report.Gaps, costCoverageGap{costResource: key.resource, Bucket: key.bucket, Metric: key.metric, From: cursor, To: window.To})
		}
	}
	if len(report.Sources) > 0 {
		report.Coverage = "partial"
		if len(report.Gaps) == 0 {
			report.Coverage = "complete"
		}
	}
	sortTotals := func(totals []monthlyCostTotal) {
		slices.SortFunc(totals, func(a, b monthlyCostTotal) int {
			return strings.Compare(a.Bucket+"\x00"+a.Currency, b.Bucket+"\x00"+b.Currency)
		})
	}
	sortTotals(report.Totals)
	slices.SortFunc(report.ByProject, func(a, b monthlyProjectCost) int { return strings.Compare(a.ProjectID, b.ProjectID) })
	for _, project := range report.ByProject {
		sortTotals(project.Totals)
	}
	slices.SortFunc(report.Gaps, func(a, b costCoverageGap) int {
		left := []string{a.ProjectID, a.Provider, a.ProviderAccount, a.ResourceID, a.ResourceName, a.Bucket, a.Metric, a.From.Format(costTimeLayout)}
		right := []string{b.ProjectID, b.Provider, b.ProviderAccount, b.ResourceID, b.ResourceName, b.Bucket, b.Metric, b.From.Format(costTimeLayout)}
		return slices.Compare(left, right)
	})
	report.SourceCount, report.GapCount, report.DetailsComplete = len(report.Sources), len(report.Gaps), true
	return report, nil
}

func boundedMonthlyCostDetails(report monthlyCostReport) monthlyCostReport {
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

func readSpriteCostResources(ctx context.Context, query nativeQueryer, organization string, projects []string) ([]costResource, error) {
	encoded, err := json.Marshal(projects)
	if err != nil {
		return nil, err
	}
	rows, err := query.QueryContext(ctx, `SELECT DISTINCT project_id,provider_account,name FROM (
SELECT project_id,provider_organization AS provider_account,name FROM project_sprite_members WHERE organization_id=? AND state<>'deleted'
UNION
SELECT g.project_id,ps.organization_slug,m.hostname FROM machines m JOIN runner_identities r ON r.machine_id=m.id AND r.removed_at IS NULL
JOIN token_grants g ON g.token_id=r.token_id AND g.organization_id=r.organization_id
JOIN project_secrets ps ON ps.organization_id=g.organization_id AND ps.project_id=g.project_id AND ps.kind='fly_sprites_token'
WHERE m.organization_id=? AND json_extract(m.capabilities_json,'$.sprite_name')=m.hostname)
WHERE (? OR project_id IN (SELECT value FROM json_each(?))) ORDER BY project_id,provider_account,name`, organization, organization, projects == nil, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("read Sprite cost inventory: %w", err)
	}
	defer rows.Close()
	resources := []costResource{}
	for rows.Next() {
		resource := costResource{Provider: "fly_sprites"}
		if err := rows.Scan(&resource.ProjectID, &resource.ProviderAccount, &resource.ResourceName); err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}

func (d *database) monthlyCosts(ctx context.Context, organization, scope string, projects []string, window usageWindow, now time.Time) (monthlyCostReport, error) {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return monthlyCostReport{}, err
	}
	defer tx.Rollback()
	observations, err := readCostObservations(ctx, tx, organization, projects, window)
	if err != nil {
		return monthlyCostReport{}, err
	}
	luna, err := readLunaCostObservations(ctx, tx, organization, projects, window)
	if err != nil {
		return monthlyCostReport{}, err
	}
	observations = append(observations, luna...)
	resources, err := readSpriteCostResources(ctx, tx, organization, projects)
	if err != nil {
		return monthlyCostReport{}, err
	}
	report, err := buildMonthlyCostReport(organization, scope, window, now, observations, resources)
	if err != nil {
		return monthlyCostReport{}, err
	}
	encoded, err := json.Marshal(projects)
	if err != nil {
		return report, err
	}
	report.RunnerReporting = "finish_time"
	err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(status='running'),0),coalesce(sum(NOT EXISTS (SELECT 1 FROM usage_cost_observations u WHERE u.organization_id=a.organization_id AND u.resource_id=a.id AND u.bucket='runner_api')),0) FROM native_attempts a WHERE organization_id=? AND substr(started_at,1,19)<?
AND (status='running' OR substr(updated_at,1,19)>? OR (substr(updated_at,1,19)=? AND length(updated_at)>20))
AND (? OR project_id IN (SELECT value FROM json_each(?)))`, organization, window.To.UTC().Format("2006-01-02T15:04:05"), window.From.UTC().Format("2006-01-02T15:04:05"), window.From.UTC().Format("2006-01-02T15:04:05"), projects == nil, string(encoded)).Scan(&report.ActiveAttempts, &report.UnreportedAttempts)
	if err != nil {
		return report, err
	}
	report.ActiveWorkComplete = report.ActiveAttempts == 0 && report.UnreportedAttempts == 0
	if !report.ActiveWorkComplete && report.Coverage == "complete" {
		report.Coverage = "partial"
	}
	return report, tx.Commit()
}
