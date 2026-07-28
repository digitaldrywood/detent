package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workload"
)

const (
	doctorAgentPoolsCheckName   = "Agent pools"
	doctorAgentPoolsWindow      = 7 * 24 * time.Hour
	doctorCloudPoolStartingSize = 10
)

type doctorWorkloadProject struct {
	ID      string
	Class   workload.Class
	Signals workload.Signals
}

type doctorAgentPoolRecommendation struct {
	Pool          string
	CurrentCap    int
	CloudCap      int
	LocalProjects []doctorWorkloadProject
	CloudProjects []doctorWorkloadProject
	Contention    []store.CrossClassPoolContention
}

func checkDoctorAgentPools(
	ctx context.Context,
	resolution globalconfig.PathResolution,
	cfg globalconfig.Config,
	deps doctorDeps,
) doctorCheck {
	if len(cfg.Global.AgentPools) > 0 {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorOK,
			Detail: configuredAgentPoolsDetail(cfg.Global.AgentPools),
		}
	}
	deps = deps.withDefaults()
	projects, classes, mixed, err := doctorClassifyWorkloadProjects(ctx, cfg, deps)
	if err != nil {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorOK,
			Detail: "skipped because not every project workflow could be classified: " + err.Error(),
		}
	}
	if !mixed {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorOK,
			Detail: "one workload class is configured; no pool split is recommended",
		}
	}
	storePath := filepath.Join(filepath.Dir(resolution.Path), "detent.db")
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return doctorCheck{
				Name:   doctorAgentPoolsCheckName,
				Status: doctorOK,
				Detail: "mixed workload classes are configured, but there is no contention telemetry yet",
			}
		}
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s contention telemetry could not be read: %v", storePath, err),
			Hint:   "Confirm the runtime database is readable, then rerun detent doctor.",
		}
	}

	contention, err := store.QueryCrossClassPoolContention(ctx, db, store.PoolContentionQuery{
		Since:          deps.now().UTC().Add(-doctorAgentPoolsWindow),
		ProjectClasses: classes,
	})
	closeErr := db.Close()
	if err != nil {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorWarn,
			Detail: "pool contention telemetry could not be analyzed: " + err.Error(),
			Hint:   "Confirm the runtime database has current work-attempt telemetry.",
		}
	}
	if closeErr != nil {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorWarn,
			Detail: "pool contention telemetry could not be closed: " + closeErr.Error(),
		}
	}
	recommendation, ok := newDoctorAgentPoolRecommendation(cfg, projects, contention)
	if !ok {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorOK,
			Detail: "mixed workload classes have no cross-class pool contention in 7d",
		}
	}
	return doctorCheck{
		Name:   doctorAgentPoolsCheckName,
		Status: doctorWarn,
		Detail: recommendation.Detail(),
		Hint:   "Review the proposed split, tune the cloud cap to provider limits, then run detent fix agent-pools.",
	}
}

func configuredAgentPoolsDetail(pools []globalconfig.AgentPool) string {
	elastic := make([]string, 0, len(pools))
	for _, pool := range pools {
		if pool.BurstTo <= pool.MaxConcurrentAgents {
			continue
		}
		elastic = append(elastic, fmt.Sprintf(
			"%s (guaranteed %d, burst ceiling %d)",
			strings.TrimSpace(pool.Name),
			pool.MaxConcurrentAgents,
			pool.BurstTo,
		))
	}
	if len(elastic) == 0 {
		return "agent pools are already configured with rigid capacity; automatic repartitioning is intentionally disabled"
	}
	return "agent pools are already configured; elastic capacity: " + strings.Join(elastic, ", ")
}

func doctorClassifyWorkloadProjects(
	ctx context.Context,
	cfg globalconfig.Config,
	deps doctorDeps,
) ([]doctorWorkloadProject, map[string]string, bool, error) {
	projects := make([]doctorWorkloadProject, 0, len(cfg.Projects))
	classes := make(map[string]string, len(cfg.Projects))
	classSet := map[workload.Class]struct{}{}
	for _, project := range cfg.Projects {
		workflowConfig, err := loadDoctorProjectWorkflow(ctx, project, deps)
		if err != nil {
			return nil, nil, false, fmt.Errorf("%s: %w", doctorProjectID(project), err)
		}
		class, signals := workload.Classify(workflowConfig.Config)
		item := doctorWorkloadProject{
			ID:      doctorProjectID(project),
			Class:   class,
			Signals: signals,
		}
		projects = append(projects, item)
		classes[item.ID] = string(class)
		classSet[class] = struct{}{}
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].ID < projects[j].ID
	})
	return projects, classes, len(classSet) > 1, nil
}

func newDoctorAgentPoolRecommendation(
	cfg globalconfig.Config,
	projects []doctorWorkloadProject,
	contention []store.CrossClassPoolContention,
) (doctorAgentPoolRecommendation, bool) {
	recommendation := doctorAgentPoolRecommendation{
		Pool:       globalconfig.DefaultAgentPoolName,
		CurrentCap: cfg.Global.MaxConcurrentAgents,
		CloudCap:   max(cfg.Global.MaxConcurrentAgents, doctorCloudPoolStartingSize),
	}
	for _, project := range projects {
		switch project.Class {
		case workload.ClassLocalHeavy:
			recommendation.LocalProjects = append(recommendation.LocalProjects, project)
		case workload.ClassCloudOnly:
			recommendation.CloudProjects = append(recommendation.CloudProjects, project)
		}
	}
	for _, item := range contention {
		if item.Pool == recommendation.Pool && item.WaitCount > 0 {
			recommendation.Contention = append(recommendation.Contention, item)
		}
	}
	return recommendation, len(recommendation.LocalProjects) > 0 &&
		len(recommendation.CloudProjects) > 0 &&
		recommendation.TotalWaits() > 0
}

func (r doctorAgentPoolRecommendation) TotalWaits() int {
	total := 0
	for _, contention := range r.Contention {
		total += contention.WaitCount
	}
	return total
}

func (r doctorAgentPoolRecommendation) Detail() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "2 workload classes share pool %q (cap %d)\n\n", r.Pool, r.CurrentCap)
	fmt.Fprintf(&builder, "  local-heavy: %s\n", strings.Join(doctorWorkloadProjectIDs(r.LocalProjects), ", "))
	builder.WriteString("    (declare local validation/build gates or CI triggers)\n")
	fmt.Fprintf(&builder, "  cloud-only:  %s\n", strings.Join(doctorWorkloadProjectIDs(r.CloudProjects), ", "))
	builder.WriteString("    (no local validation/build gate, no CI trigger)\n\n")
	fmt.Fprintf(&builder, "  cross-class work waited on pool capacity %d times in 7d.\n", r.TotalWaits())
	for _, contention := range r.Contention {
		fmt.Fprintf(
			&builder,
			"  %s waited %d time(s); %s held a slot.\n",
			contention.WaitingClass,
			contention.WaitCount,
			contention.HoldingClass,
		)
	}
	builder.WriteString("\n  suggested:\n")
	for _, line := range strings.Split(strings.TrimRight(r.SuggestedYAML(), "\n"), "\n") {
		builder.WriteString("    ")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return strings.TrimRight(builder.String(), "\n")
}

func (r doctorAgentPoolRecommendation) SuggestedYAML() string {
	var builder strings.Builder
	builder.WriteString("global:\n")
	builder.WriteString("  agent_pools:\n")
	builder.WriteString("    - name: code\n")
	fmt.Fprintf(&builder, "      max_concurrent_agents: %d\n", r.CurrentCap)
	builder.WriteString("    - name: cloud\n")
	fmt.Fprintf(&builder, "      max_concurrent_agents: %d # tune to your provider limits\n", r.CloudCap)
	builder.WriteString("projects:\n")
	for _, project := range r.LocalProjects {
		fmt.Fprintf(&builder, "  - id: %s\n    pool: code\n", doctorAgentPoolYAMLString(project.ID))
	}
	for _, project := range r.CloudProjects {
		fmt.Fprintf(&builder, "  - id: %s\n    pool: cloud\n", doctorAgentPoolYAMLString(project.ID))
	}
	return builder.String()
}

func doctorAgentPoolYAMLString(value string) string {
	encoded, err := yaml.Marshal(&yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: value,
		Style: yaml.DoubleQuotedStyle,
	})
	if err != nil {
		return strconv.Quote(value)
	}
	return strings.TrimSpace(string(encoded))
}

func doctorWorkloadProjectIDs(projects []doctorWorkloadProject) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return ids
}

func (r doctorAgentPoolRecommendation) PoolForProject(projectID string) string {
	for _, project := range r.LocalProjects {
		if project.ID == projectID {
			return "code"
		}
	}
	for _, project := range r.CloudProjects {
		if project.ID == projectID {
			return "cloud"
		}
	}
	return ""
}
