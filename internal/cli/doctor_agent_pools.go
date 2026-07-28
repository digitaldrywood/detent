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

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workload"
)

const (
	doctorAgentPoolsCheckName   = "Capacity constraints"
	doctorAgentPoolsWindow      = 7 * 24 * time.Hour
	doctorCloudPoolStartingSize = 10
)

type doctorWorkloadProject struct {
	ID       string
	Pool     string
	Class    workload.Class
	Signals  workload.Signals
	Workflow workflowconfig.Config
}

type doctorAgentPoolRecommendation struct {
	Pool          string
	CurrentCap    int
	CloudCap      int
	PoolWaits     int
	LocalProjects []doctorWorkloadProject
	CloudProjects []doctorWorkloadProject
	Contention    []store.CrossClassPoolContention
}

type doctorCapacityRow struct {
	Reason store.CapacityConstraintReason
	Lane   string
	Count  int
}

type doctorCapacityFinding struct {
	Project doctorWorkloadProject
	Pool    string
	Rows    []doctorCapacityRow
	Binding doctorCapacityRow
	Total   int
}

func checkDoctorAgentPools(
	ctx context.Context,
	resolution globalconfig.PathResolution,
	cfg globalconfig.Config,
	deps doctorDeps,
) doctorCheck {
	deps = deps.withDefaults()
	projects, classes, _, err := doctorClassifyWorkloadProjects(ctx, cfg, deps)
	if err != nil {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorOK,
			Detail: "skipped because not every project workflow could be classified: " + err.Error(),
		}
	}
	coherence := doctorPoolCoherenceFindings(cfg, projects)
	storePath := filepath.Join(filepath.Dir(resolution.Path), "detent.db")
	db, err := deps.openSQLiteReadOnly(ctx, storePath)
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			return newDoctorCapacityCheck(cfg, projects, nil, nil, coherence)
		}
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorWarn,
			Detail: fmt.Sprintf("%s contention telemetry could not be read: %v", storePath, err),
			Hint:   "Confirm the runtime database is readable, then rerun detent doctor.",
		}
	}

	waits, waitErr := store.QueryCapacityConstraintWaits(ctx, db, store.CapacityConstraintQuery{
		Since:          deps.now().UTC().Add(-doctorAgentPoolsWindow),
		ProjectClasses: classes,
	})
	contention, err := store.QueryCrossClassPoolContention(ctx, db, store.PoolContentionQuery{
		Since:          deps.now().UTC().Add(-doctorAgentPoolsWindow),
		ProjectClasses: classes,
	})
	closeErr := db.Close()
	if waitErr != nil {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorWarn,
			Detail: "capacity constraint telemetry could not be analyzed: " + waitErr.Error(),
			Hint:   "Confirm the runtime database has current scheduler-decision telemetry.",
		}
	}
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
	return newDoctorCapacityCheck(cfg, projects, waits, contention, coherence)
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
			ID:       doctorProjectID(project),
			Pool:     doctorProjectPool(project),
			Class:    class,
			Signals:  signals,
			Workflow: workflowConfig.Config,
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

func newDoctorCapacityCheck(
	cfg globalconfig.Config,
	projects []doctorWorkloadProject,
	waits []store.CapacityConstraintWait,
	contention []store.CrossClassPoolContention,
	coherence []string,
) doctorCheck {
	findings := newDoctorCapacityFindings(projects, waits)
	if len(findings) == 0 && len(coherence) == 0 {
		return doctorCheck{
			Name:   doctorAgentPoolsCheckName,
			Status: doctorOK,
			Detail: "no binding capacity constraint detected",
		}
	}

	var builder strings.Builder
	hint := "Review the binding constraint before changing capacity."
	for index, finding := range findings {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		fmt.Fprintf(
			&builder,
			"%s (%s) blocked %s in 7d\n",
			finding.Project.ID,
			finding.Project.Class,
			doctorCountTimes(finding.Total),
		)
		for _, row := range finding.Rows {
			marker := ""
			if row.Reason == finding.Binding.Reason && row.Lane == finding.Binding.Lane {
				marker = "  <- binding"
			}
			fmt.Fprintf(&builder, "  %-48s %6d%s\n", doctorCapacityRowLabel(row), row.Count, marker)
		}
		recommendation, recommendationHint := doctorCapacityRecommendation(cfg, projects, finding, contention)
		if recommendation != "" {
			builder.WriteByte('\n')
			for _, line := range strings.Split(recommendation, "\n") {
				builder.WriteString("  ")
				builder.WriteString(line)
				builder.WriteByte('\n')
			}
		}
		if recommendationHint != "" {
			hint = recommendationHint
		}
	}
	if len(coherence) > 0 {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString("static configuration cannot deliver its declared capacity:\n")
		for _, finding := range coherence {
			builder.WriteString("  - ")
			builder.WriteString(finding)
			builder.WriteByte('\n')
		}
		if len(findings) == 0 {
			hint = "Align pool, project, and active-lane caps before raising capacity."
		}
	}

	return doctorCheck{
		Name:   doctorAgentPoolsCheckName,
		Status: doctorWarn,
		Detail: strings.TrimRight(builder.String(), "\n"),
		Hint:   hint,
	}
}

func newDoctorCapacityFindings(
	projects []doctorWorkloadProject,
	waits []store.CapacityConstraintWait,
) []doctorCapacityFinding {
	projectsByID := make(map[string]doctorWorkloadProject, len(projects))
	for _, project := range projects {
		projectsByID[project.ID] = project
	}
	type findingBuilder struct {
		project doctorWorkloadProject
		pool    string
		counts  map[string]doctorCapacityRow
		total   int
	}
	builders := map[string]*findingBuilder{}
	for _, wait := range waits {
		if wait.WaitCount <= 0 {
			continue
		}
		project, ok := projectsByID[wait.ProjectID]
		if !ok || wait.Pool != project.Pool {
			continue
		}
		builder := builders[wait.ProjectID]
		if builder == nil {
			builder = &findingBuilder{
				project: project,
				pool:    wait.Pool,
				counts:  map[string]doctorCapacityRow{},
			}
			builders[wait.ProjectID] = builder
		}
		lane := ""
		if wait.Reason == store.CapacityConstraintLane {
			lane = strings.TrimSpace(wait.Lane)
		}
		key := string(wait.Reason) + "\x00" + strings.ToLower(lane)
		row := builder.counts[key]
		row.Reason = wait.Reason
		row.Lane = lane
		row.Count += wait.WaitCount
		builder.counts[key] = row
		builder.total += wait.WaitCount
	}

	findings := make([]doctorCapacityFinding, 0, len(builders))
	for _, builder := range builders {
		rows := doctorCapacityRows(builder.counts)
		findings = append(findings, doctorCapacityFinding{
			Project: builder.project,
			Pool:    builder.pool,
			Rows:    rows,
			Binding: rows[0],
			Total:   builder.total,
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		return findings[i].Project.ID < findings[j].Project.ID
	})
	return findings
}

func doctorCapacityRows(counts map[string]doctorCapacityRow) []doctorCapacityRow {
	for _, reason := range []store.CapacityConstraintReason{
		store.CapacityConstraintPool,
		store.CapacityConstraintProject,
		store.CapacityConstraintLane,
		store.CapacityConstraintWorkerHost,
		store.CapacityConstraintRateWindow,
	} {
		found := false
		for _, row := range counts {
			if row.Reason == reason {
				found = true
				break
			}
		}
		if !found {
			counts[string(reason)+"\x00"] = doctorCapacityRow{Reason: reason}
		}
	}
	rows := make([]doctorCapacityRow, 0, len(counts))
	for _, row := range counts {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if doctorCapacityReasonPriority(rows[i].Reason) != doctorCapacityReasonPriority(rows[j].Reason) {
			return doctorCapacityReasonPriority(rows[i].Reason) < doctorCapacityReasonPriority(rows[j].Reason)
		}
		return strings.ToLower(rows[i].Lane) < strings.ToLower(rows[j].Lane)
	})
	return rows
}

func doctorCapacityReasonPriority(reason store.CapacityConstraintReason) int {
	switch reason {
	case store.CapacityConstraintRateWindow:
		return 0
	case store.CapacityConstraintPool:
		return 1
	case store.CapacityConstraintProject:
		return 2
	case store.CapacityConstraintLane:
		return 3
	case store.CapacityConstraintWorkerHost:
		return 4
	default:
		return 5
	}
}

func doctorCapacityRowLabel(row doctorCapacityRow) string {
	if row.Reason == store.CapacityConstraintPool {
		return "pool waits"
	}
	if row.Reason == store.CapacityConstraintLane && strings.TrimSpace(row.Lane) != "" {
		return fmt.Sprintf("%s (%s)", row.Reason, row.Lane)
	}
	return string(row.Reason)
}

func doctorCapacityRecommendation(
	cfg globalconfig.Config,
	projects []doctorWorkloadProject,
	finding doctorCapacityFinding,
	contention []store.CrossClassPoolContention,
) (string, string) {
	projectID := finding.Project.ID
	switch finding.Binding.Reason {
	case store.CapacityConstraintPool:
		if doctorPoolHasMixedClasses(projects, finding.Pool) {
			if len(cfg.Global.AgentPools) == 0 && finding.Pool == globalconfig.DefaultAgentPoolName {
				recommendation, ok := newDoctorAgentPoolRecommendationForWaits(
					cfg,
					projects,
					contention,
					finding.Binding.Count,
				)
				if ok {
					return recommendation.Detail(),
						"Review the proposed split, tune the cloud cap to provider limits, then run detent fix agent-pools."
				}
			}
			return fmt.Sprintf(
				"split pool %q by workload class; configured pools require an operator-reviewed repartition.",
				finding.Pool,
			), ""
		}
		return fmt.Sprintf("raise %s; this pool contains one workload class, so do not split it.", doctorPoolCapacityPath(cfg, finding.Pool)), ""
	case store.CapacityConstraintProject:
		return fmt.Sprintf(
			"the pool cap is not throttling this workload.\nraise project %q agent.max_concurrent_agents.",
			projectID,
		), ""
	case store.CapacityConstraintLane:
		path := "agent.max_concurrent_agents_by_state"
		if strings.TrimSpace(finding.Binding.Lane) != "" {
			path += "." + finding.Binding.Lane
		}
		return fmt.Sprintf(
			"the pool cap is not throttling this workload.\nraise project %q %s before reaching for a pool split.",
			projectID,
			path,
		), ""
	case store.CapacityConstraintWorkerHost:
		return fmt.Sprintf(
			"the pool cap is not throttling this workload.\nraise project %q worker.max_concurrent_agents_per_host.",
			projectID,
		), ""
	case store.CapacityConstraintRateWindow:
		return "provider rate-window backpressure is binding; no config change is recommended because raising a cap will not help.", ""
	default:
		return "", ""
	}
}

func doctorPoolCoherenceFindings(
	cfg globalconfig.Config,
	projects []doctorWorkloadProject,
) []string {
	poolCaps := map[string]int{
		globalconfig.DefaultAgentPoolName: cfg.Global.MaxConcurrentAgents,
	}
	for _, pool := range cfg.Global.AgentPools {
		poolCaps[strings.TrimSpace(pool.Name)] = pool.MaxConcurrentAgents
	}
	memberCapSums := map[string]int{}
	memberCounts := map[string]int{}
	var findings []string
	for _, project := range projects {
		poolCap, ok := poolCaps[project.Pool]
		if !ok {
			continue
		}
		projectCap := project.Workflow.Agent.MaxConcurrentAgents
		memberCapSums[project.Pool] += projectCap
		memberCounts[project.Pool]++
		if projectCap > poolCap {
			findings = append(findings, fmt.Sprintf(
				"project %q agent.max_concurrent_agents=%d exceeds pool %q capacity %d; the project cap is dead configuration",
				project.ID,
				projectCap,
				project.Pool,
				poolCap,
			))
		}
		lanes := make([]string, 0, len(project.Workflow.Agent.MaxConcurrentAgentsByState))
		for lane := range project.Workflow.Agent.MaxConcurrentAgentsByState {
			lanes = append(lanes, lane)
		}
		sort.Strings(lanes)
		for _, lane := range lanes {
			laneCap := project.Workflow.Agent.MaxConcurrentAgentsByState[lane]
			if laneCap >= projectCap || !doctorActiveWorkLane(project.Workflow, lane) {
				continue
			}
			findings = append(findings, fmt.Sprintf(
				"project %q active lane %q cap %d binds below agent.max_concurrent_agents=%d",
				project.ID,
				lane,
				laneCap,
				projectCap,
			))
		}
	}
	pools := make([]string, 0, len(memberCounts))
	for pool := range memberCounts {
		pools = append(pools, pool)
	}
	sort.Strings(pools)
	for _, pool := range pools {
		if memberCapSums[pool] >= poolCaps[pool] {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"pool %q capacity %d exceeds its %d member project cap total %d; the pool can never fill",
			pool,
			poolCaps[pool],
			memberCounts[pool],
			memberCapSums[pool],
		))
	}
	sort.Strings(findings)
	return findings
}

func doctorActiveWorkLane(cfg workflowconfig.Config, lane string) bool {
	normalized := strings.ToLower(strings.TrimSpace(lane))
	if normalized == "" || normalized == "merging" {
		return false
	}
	for _, active := range cfg.Tracker.ActiveStates {
		if strings.EqualFold(strings.TrimSpace(active), lane) {
			return true
		}
	}
	return false
}

func doctorPoolHasMixedClasses(projects []doctorWorkloadProject, pool string) bool {
	classes := map[workload.Class]struct{}{}
	for _, project := range projects {
		if project.Pool == pool {
			classes[project.Class] = struct{}{}
		}
	}
	return len(classes) > 1
}

func doctorPoolCapacityPath(cfg globalconfig.Config, pool string) string {
	if pool == globalconfig.DefaultAgentPoolName {
		return "global.max_concurrent_agents"
	}
	for _, configured := range cfg.Global.AgentPools {
		if strings.TrimSpace(configured.Name) == pool {
			return fmt.Sprintf("global.agent_pools[%q].max_concurrent_agents", pool)
		}
	}
	return fmt.Sprintf("pool %q capacity", pool)
}

func doctorProjectPool(project globalconfig.Project) string {
	pool := strings.TrimSpace(project.Pool)
	if pool == "" {
		return globalconfig.DefaultAgentPoolName
	}
	return pool
}

func doctorCountTimes(count int) string {
	if count == 1 {
		return "1x"
	}
	return fmt.Sprintf("%dx", count)
}

func newDoctorAgentPoolRecommendationForWaits(
	cfg globalconfig.Config,
	projects []doctorWorkloadProject,
	contention []store.CrossClassPoolContention,
	poolWaits int,
) (doctorAgentPoolRecommendation, bool) {
	recommendation := doctorAgentPoolRecommendation{
		Pool:       globalconfig.DefaultAgentPoolName,
		CurrentCap: cfg.Global.MaxConcurrentAgents,
		CloudCap:   max(cfg.Global.MaxConcurrentAgents, doctorCloudPoolStartingSize),
		PoolWaits:  poolWaits,
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
	if total == 0 {
		total = r.PoolWaits
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
	if len(r.Contention) > 0 {
		fmt.Fprintf(&builder, "  cross-class work waited on pool capacity %d times in 7d.\n", r.TotalWaits())
	} else {
		fmt.Fprintf(&builder, "  mixed workload classes waited on pool capacity %d times in 7d.\n", r.TotalWaits())
	}
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
