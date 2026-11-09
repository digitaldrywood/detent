package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const healthEvaluationMaintenanceBytes = 512 << 20

type healthEvaluationFinding struct {
	Finding  healthFinding `json:"finding"`
	Projects []string      `json:"projects"`
	Since    time.Time     `json:"since,omitzero"`
}

type healthEvaluationSource struct {
	Baselines []healthBaselineUnavailable
	Gaps      []healthObservationGap
}

type healthEvaluationInput struct {
	Gaps      []healthObservationGap      `json:"gaps,omitempty"`
	Schema    int                         `json:"schema"`
	At        time.Time                   `json:"at"`
	Findings  []healthEvaluationFinding   `json:"findings"`
	Baselines []healthBaselineUnavailable `json:"baselines"`
}

func newHealthEvaluationInput(at time.Time, findings []healthFinding, source healthEvaluationSource) (healthEvaluationInput, error) {
	input := healthEvaluationInput{Schema: 1, At: at.UTC().Truncate(time.Microsecond), Baselines: slices.Clone(source.Baselines), Gaps: slices.Clone(source.Gaps)}
	if input.At.IsZero() || len(findings) > healthReadLimit || len(source.Baselines) > healthReadLimit || len(source.Gaps) > healthReadLimit {
		return input, errHealthReadLimit
	}
	seen := map[string]bool{}
	for _, finding := range findings {
		if finding.Fingerprint == "" || seen[finding.Fingerprint] {
			return input, errors.New("invalid health fingerprint set")
		}
		seen[finding.Fingerprint] = true
		projects := slices.Clone(finding.Projects)
		slices.Sort(projects)
		projects = slices.Compact(projects)
		if slices.Contains(projects, "") {
			return input, corestore.ErrInvalidPage
		}
		input.Findings = append(input.Findings, healthEvaluationFinding{Finding: finding, Projects: projects, Since: finding.ConditionSince})
	}
	slices.SortFunc(input.Baselines, func(a, b healthBaselineUnavailable) int {
		if a.Project != b.Project {
			return strings.Compare(a.Project, b.Project)
		}
		if a.Signal != b.Signal {
			return strings.Compare(a.Signal, b.Signal)
		}
		return strings.Compare(a.Lane, b.Lane)
	})
	input.Baselines = slices.Compact(input.Baselines)
	for _, baseline := range input.Baselines {
		if baseline.Project == "" || baseline.Signal == "" {
			return input, corestore.ErrInvalidPage
		}
	}
	slices.SortFunc(input.Gaps, func(a, b healthObservationGap) int {
		if a.Project != b.Project {
			return strings.Compare(a.Project, b.Project)
		}
		return strings.Compare(a.Signal, b.Signal)
	})
	input.Gaps = slices.Compact(input.Gaps)
	for _, gap := range input.Gaps {
		if gap.Project == "" || gap.Signal != "cost_anomaly" {
			return input, corestore.ErrInvalidPage
		}
	}
	if len(input.projects()) > healthProjectLimit {
		return input, errHealthReadLimit
	}
	return input, nil
}

func (input healthEvaluationInput) projects() []tracker.ProjectID {
	ids := map[tracker.ProjectID]bool{}
	for _, finding := range input.Findings {
		for _, project := range finding.Projects {
			ids[tracker.ProjectID(project)] = true
		}
	}
	for _, baseline := range input.Baselines {
		ids[tracker.ProjectID(baseline.Project)] = true
	}
	for _, gap := range input.Gaps {
		ids[tracker.ProjectID(gap.Project)] = true
	}
	result := make([]tracker.ProjectID, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}

func (s *Service) HealthEvaluationMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "health-evaluation", Bytes: healthEvaluationMaintenanceBytes, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		if err := hubmaintenance.RequireAllowance(ctx, tenant.Organization, healthEvaluationMaintenanceBytes); err != nil {
			return err
		}
		if _, ok := s.runDetailStore().(*hubmaintenance.Details); !ok {
			return errors.New("maintenance requires byte-limited payload storage")
		}
		organization := tracker.OrganizationID(tenant.Organization)
		var exists bool
		var tick time.Time
		err := s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
			owner, ok := r.(healthstore.EvaluationReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			_, err := owner.HealthEvaluation(ctx)
			if err == nil {
				exists = true
				return nil
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			tick, err = r.HealthTick(ctx)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			return err
		})
		if err != nil {
			return err
		}
		if exists {
			_, err := s.advanceHealthEvaluation(ctx, organization, budget.Items)
			return err
		}
		now := s.config.now().UTC().Truncate(time.Microsecond)
		if !tick.IsZero() && now.Before(tick.Add(healthInterval)) {
			return nil
		}
		snapshot, err := s.readHealthSnapshotRecords(ctx, organization, now)
		if err != nil {
			return err
		}
		return s.publishHealthEvaluation(ctx, organization, now, evaluateHealth(now, snapshot), healthEvaluationSource{Baselines: snapshot.BaselineUnavailable, Gaps: snapshot.Incomplete})
	}}
}

func (s *Service) readHealthEvaluationInput(ctx context.Context, work healthstore.Evaluation) (healthEvaluationInput, error) {
	var input healthEvaluationInput
	raw, err := s.runDetailStore().ReadDetail(ctx, string(work.OrganizationID), work.Input)
	if err != nil {
		return input, fmt.Errorf("read health evaluation input: %w", err)
	}
	if json.Unmarshal(raw, &input) != nil || input.Schema != 1 || !input.At.Equal(work.At) || int64(len(input.Findings)) != work.FindingCount || int64(len(input.Baselines)) != work.BaselineCount {
		return input, artifact.ErrIntegrity
	}
	findings := make([]healthFinding, 0, len(input.Findings))
	for _, entry := range input.Findings {
		finding := entry.Finding
		finding.Projects, finding.ConditionSince = entry.Projects, entry.Since
		findings = append(findings, finding)
	}
	verified, err := newHealthEvaluationInput(input.At, findings, healthEvaluationSource{Baselines: input.Baselines, Gaps: input.Gaps})
	if err != nil {
		return input, artifact.ErrIntegrity
	}
	return verified, nil
}

func (s *Service) advanceHealthEvaluation(ctx context.Context, organization tracker.OrganizationID, limit int) (bool, error) {
	if limit < 1 || limit > hubmaintenance.ItemLimit {
		return false, corestore.ErrInvalidPage
	}
	var work healthstore.Evaluation
	err := s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
		owner, ok := r.(healthstore.EvaluationReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		work, err = owner.HealthEvaluation(ctx)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	input, err := s.readHealthEvaluationInput(ctx, work)
	if err != nil {
		return false, err
	}
	var projects []tracker.ProjectID
	err = s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
		owner, ok := r.(healthstore.EvaluationReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		projects, err = owner.HealthEvaluationProjects(ctx, input.projects())
		return err
	})
	if err != nil {
		return false, err
	}
	active := map[string]bool{}
	for _, project := range projects {
		active[string(project)] = true
	}
	seen := map[string]bool{}
	for _, entry := range input.Findings {
		if len(entry.Projects) == 0 || slices.ContainsFunc(entry.Projects, func(project string) bool { return active[project] }) {
			seen[entry.Finding.Fingerprint] = true
		}
	}
	wanted := map[healthstore.Baseline]bool{}
	for _, baseline := range input.Baselines {
		if active[baseline.Project] {
			wanted[healthstore.Baseline{ProjectID: tracker.ProjectID(baseline.Project), Signal: baseline.Signal, Lane: baseline.Lane}] = true
		}
	}
	gaps := map[string][]tracker.ProjectID{}
	for _, gap := range input.Gaps {
		if active[gap.Project] {
			gaps[gap.Signal] = append(gaps[gap.Signal], tracker.ProjectID(gap.Project))
		}
	}
	for range limit {
		next := work.EvaluationProgress
		var mutate func(postgres.HealthWriter) error
		finish := false
		switch work.Phase {
		case "publish":
			if work.Position < work.FindingCount {
				entry := input.Findings[work.Position]
				finding := entry.Finding
				finding.Projects, finding.ConditionSince = nil, entry.Since
				for _, project := range entry.Projects {
					if active[project] {
						finding.Projects = append(finding.Projects, project)
					}
				}
				if len(entry.Projects) == 0 || len(finding.Projects) > 0 {
					raw, err := json.Marshal(finding.Evidence)
					if err != nil {
						return false, fmt.Errorf("encode health evidence: %w", err)
					}
					ref, err := s.runDetailStore().StoreDetail(ctx, string(organization), raw)
					if err != nil {
						return false, fmt.Errorf("store health evidence: %w", err)
					}
					mutate = func(w postgres.HealthWriter) error {
						return s.publishHealthFinding(ctx, w, organization, work.At, finding, ref)
					}
				}
				next.Position++
			} else {
				next = healthstore.EvaluationProgress{Phase: "resolve"}
			}
		case "resolve":
			var rows []postgres.HealthRecord
			err := s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
				var err error
				rows, err = r.HealthEvaluationRecords(ctx, postgres.HealthEvaluationRecordsInput{OrganizationID: organization, ID: work.FindingCursor, Limit: 1})
				if err != nil || len(rows) == 0 {
					return err
				}
				row := rows[0]
				if !seen[row.Fingerprint] && !row.LastSeenAt.After(work.At) && len(gaps[row.Signal]) > 0 {
					owner, ok := r.(healthstore.EvaluationReader)
					if !ok {
						return workItemStorageUnavailable()
					}
					retained, err := owner.HealthEvaluationHasProjectLink(ctx, row.ID, gaps[row.Signal])
					if err != nil {
						return err
					}
					if retained {
						seen[row.Fingerprint] = true
					}
				}
				return nil
			})
			if err != nil {
				return false, err
			}
			if len(rows) == 0 {
				next = healthstore.EvaluationProgress{Phase: "baseline_clear"}
			} else {
				row := rows[0]
				if !seen[row.Fingerprint] && !row.LastSeenAt.After(work.At) {
					mutate = func(w postgres.HealthWriter) error { return s.resolveHealthRecord(ctx, w, organization, work.At, row) }
				}
				next.FindingCursor = row.ID
			}
		case "baseline_clear":
			var rows []healthstore.Baseline
			err := s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
				owner, ok := r.(healthstore.EvaluationReader)
				if !ok {
					return workItemStorageUnavailable()
				}
				var err error
				rows, err = owner.HealthEvaluationBaselines(ctx, work.BaselineCursor, 1)
				return err
			})
			if err != nil {
				return false, err
			}
			if len(rows) == 0 {
				next = healthstore.EvaluationProgress{Phase: "baseline_put"}
			} else {
				baseline := rows[0]
				if !wanted[baseline] {
					mutate = func(w postgres.HealthWriter) error {
						return w.ClearHealthBaseline(ctx, postgres.ClearHealthBaselineInput{OrganizationID: organization, ProjectID: baseline.ProjectID, Signal: baseline.Signal, Lane: baseline.Lane})
					}
				}
				next.BaselineCursor = baseline
			}
		case "baseline_put":
			if work.Position < work.BaselineCount {
				baseline := input.Baselines[work.Position]
				if active[baseline.Project] {
					mutate = func(w postgres.HealthWriter) error {
						return w.PutHealthBaseline(ctx, postgres.PutHealthBaselineInput{OrganizationID: organization, ProjectID: tracker.ProjectID(baseline.Project), Signal: baseline.Signal, Lane: baseline.Lane})
					}
				}
				next.Position++
			} else {
				finish = true
			}
		default:
			return false, artifact.ErrIntegrity
		}
		err := s.database.postgres.WriteHealth(ctx, organization, corestore.Background, func(w postgres.HealthWriter) error {
			owner, ok := w.(healthstore.EvaluationWriter)
			if !ok {
				return workItemStorageUnavailable()
			}
			if _, err := owner.LockHealthEvaluation(ctx, work); err != nil {
				return err
			}
			if mutate != nil {
				if err := mutate(w); err != nil {
					return err
				}
			}
			if finish {
				return owner.FinishHealthEvaluation(ctx, work)
			}
			var err error
			work, err = owner.AdvanceHealthEvaluation(ctx, work, next)
			return err
		})
		if err != nil {
			return false, err
		}
		if finish {
			return true, nil
		}
	}
	return true, nil
}
