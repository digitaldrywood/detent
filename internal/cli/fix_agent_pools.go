package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/store"
)

const configFileMode = 0o600

type agentPoolsFixResult struct {
	Path          string `json:"path"`
	Diff          string `json:"diff,omitempty"`
	SuggestedYAML string `json:"suggested_yaml,omitempty"`
	Message       string `json:"message"`
	DryRun        bool   `json:"dry_run"`
	Applied       bool   `json:"applied"`
	Cancelled     bool   `json:"cancelled"`
	Noop          bool   `json:"noop"`
}

type agentPoolsFixPlan struct {
	Path           string
	Before         []byte
	After          []byte
	Diff           string
	Recommendation doctorAgentPoolRecommendation
	Noop           bool
	Message        string
}

func newAgentPoolsFixCommand(configPath *string, opts options) *cobra.Command {
	return newAgentPoolsFixCommandWithDeps(configPath, opts, doctorDeps{})
}

func newAgentPoolsFixCommandWithDeps(configPath *string, opts options, deps doctorDeps) *cobra.Command {
	var dryRun bool
	var confirmed bool
	cmd := &cobra.Command{
		Use:          "agent-pools",
		Short:        "Apply the doctor-recommended agent pool split",
		Example:      "detent fix agent-pools --dry-run\n  detent fix agent-pools --yes",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun && confirmed {
				return WrapValidation(errors.New("--dry-run and --yes cannot be used together"))
			}
			plan, err := planAgentPoolsFix(cmd.Context(), derefString(configPath), opts, deps)
			if err != nil {
				return err
			}
			result := agentPoolsFixResult{
				Path:    plan.Path,
				Diff:    plan.Diff,
				Message: plan.Message,
				DryRun:  dryRun,
				Noop:    plan.Noop,
			}
			if !plan.Noop {
				result.SuggestedYAML = plan.Recommendation.SuggestedYAML()
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			if plan.Noop || dryRun {
				return out.Write(func(writer io.Writer) error {
					return writeAgentPoolsFixResult(writer, result)
				}, result)
			}
			if !confirmed {
				if !out.IsJSON() {
					if err := writeAgentPoolsFixResult(cmd.OutOrStdout(), result); err != nil {
						return err
					}
				}
				ok, err := confirmFix(cmd, "Apply this agent-pool split?", "agent-pool")
				if err != nil {
					return err
				}
				if !ok {
					result.Cancelled = true
					result.Message = "Agent-pool fix cancelled; no files changed."
					if out.IsJSON() {
						return out.Write(nil, result)
					}
					_, err := fmt.Fprintln(cmd.OutOrStdout(), result.Message)
					return err
				}
			}
			if err := applyAgentPoolsFix(plan); err != nil {
				return err
			}
			result.Applied = true
			result.Message = "Agent-pool split applied; global config reload will take effect without a restart."
			return out.Write(func(writer io.Writer) error {
				return writeAgentPoolsFixResult(writer, result)
			}, result)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the recommended global.yaml diff without writing")
	cmd.Flags().BoolVar(&confirmed, "yes", false, "apply the split without an interactive confirmation")
	return cmd
}

func planAgentPoolsFix(
	ctx context.Context,
	configPath string,
	opts options,
	deps doctorDeps,
) (agentPoolsFixPlan, error) {
	defaults := defaultOptions()
	if opts.resolvePath == nil {
		opts.resolvePath = defaults.resolvePath
	}
	if opts.read == nil {
		opts.read = defaults.read
	}
	resolution, err := opts.resolvePath(configPath)
	if err != nil {
		return agentPoolsFixPlan{}, err
	}
	cfg, err := opts.read(resolution.Path)
	if err != nil {
		return agentPoolsFixPlan{}, err
	}
	plan := agentPoolsFixPlan{Path: resolution.Path}
	if len(cfg.Global.AgentPools) > 0 {
		plan.Noop = true
		plan.Message = "No changes made: global.agent_pools is already configured; repartitioning requires an operator decision."
		return plan, nil
	}

	deps = deps.withDefaults()
	projects, classes, mixed, err := doctorClassifyWorkloadProjects(ctx, cfg, deps)
	if err != nil {
		return agentPoolsFixPlan{}, err
	}
	if !mixed {
		plan.Noop = true
		plan.Message = "No changes needed: doctor sees only one workload class."
		return plan, nil
	}

	db, err := deps.openSQLiteReadOnly(ctx, doctorRuntimeStorePath(resolution.Path))
	if err != nil {
		if errors.Is(err, errDoctorTelemetryStoreUnavailable) {
			plan.Noop = true
			plan.Message = "No changes needed: doctor has no pool-contention telemetry yet."
			return plan, nil
		}
		return agentPoolsFixPlan{}, err
	}
	contention, queryErr := store.QueryCrossClassPoolContention(ctx, db, store.PoolContentionQuery{
		Since:          deps.now().UTC().Add(-doctorAgentPoolsWindow),
		ProjectClasses: classes,
	})
	closeErr := db.Close()
	if queryErr != nil {
		return agentPoolsFixPlan{}, queryErr
	}
	if closeErr != nil {
		return agentPoolsFixPlan{}, closeErr
	}
	recommendation, ok := newDoctorAgentPoolRecommendation(cfg, projects, contention)
	if !ok {
		plan.Noop = true
		plan.Message = "No changes needed: doctor found no cross-class pool contention in 7d."
		return plan, nil
	}

	raw, err := os.ReadFile(resolution.Path)
	if err != nil {
		return agentPoolsFixPlan{}, fmt.Errorf("read global config %s: %w", resolution.Path, err)
	}
	updated, err := applyAgentPoolRecommendationToYAML(raw, recommendation)
	if err != nil {
		return agentPoolsFixPlan{}, err
	}
	if _, err := globalconfig.Parse(updated, resolution.Path); err != nil {
		return agentPoolsFixPlan{}, fmt.Errorf("validate recommended global config: %w", err)
	}
	plan.Before = raw
	plan.After = updated
	plan.Recommendation = recommendation
	plan.Diff = agentPoolsFixDiff(resolution.Path, recommendation)
	plan.Message = "Doctor found mixed workloads with cross-class pool contention."
	return plan, nil
}

func applyAgentPoolRecommendationToYAML(
	raw []byte,
	recommendation doctorAgentPoolRecommendation,
) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse global config YAML: %w", err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("global config root must be a mapping")
	}
	root := document.Content[0]
	global := yamlMappingValue(root, "global")
	if global == nil || global.Kind != yaml.MappingNode {
		return nil, errors.New("global config global key must be a mapping")
	}
	if yamlMappingValue(global, "agent_pools") != nil {
		return nil, errors.New("global.agent_pools is already configured")
	}
	global.Content = append(global.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "agent_pools"},
		agentPoolsYAMLNode(recommendation),
	)

	projects := yamlMappingValue(root, "projects")
	if projects == nil || projects.Kind != yaml.SequenceNode {
		return nil, errors.New("global config projects key must be a sequence")
	}
	for _, project := range projects.Content {
		if project.Kind != yaml.MappingNode {
			return nil, errors.New("global config project must be a mapping")
		}
		idNode := yamlMappingValue(project, "id")
		if idNode == nil {
			return nil, errors.New("global config project id is required")
		}
		pool := recommendation.PoolForProject(strings.TrimSpace(idNode.Value))
		if pool == "" {
			continue
		}
		if yamlMappingValue(project, "pool") != nil {
			return nil, fmt.Errorf("project %s already declares a pool", idNode.Value)
		}
		project.Content = append(project.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "pool"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: pool},
		)
	}

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, fmt.Errorf("encode global config YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close global config YAML encoder: %w", err)
	}
	return output.Bytes(), nil
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func agentPoolsYAMLNode(recommendation doctorAgentPoolRecommendation) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.SequenceNode,
		Tag:  "!!seq",
		Content: []*yaml.Node{
			agentPoolYAMLNode("code", recommendation.CurrentCap, ""),
			agentPoolYAMLNode("cloud", recommendation.CloudCap, "tune to your provider limits"),
		},
	}
}

func agentPoolYAMLNode(name string, capacity int, comment string) *yaml.Node {
	capacityNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(capacity)}
	capacityNode.LineComment = comment
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "max_concurrent_agents"},
			capacityNode,
		},
	}
}

func agentPoolsFixDiff(path string, recommendation doctorAgentPoolRecommendation) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "--- %s\n+++ %s\n", path, path)
	builder.WriteString("@@ global.agent_pools @@\n")
	for _, line := range strings.Split(strings.TrimRight(recommendation.SuggestedYAML(), "\n"), "\n") {
		if line == "projects:" {
			break
		}
		if line == "global:" {
			continue
		}
		builder.WriteByte('+')
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	builder.WriteString("@@ projects[].pool @@\n")
	for _, project := range append(
		append([]doctorWorkloadProject(nil), recommendation.LocalProjects...),
		recommendation.CloudProjects...,
	) {
		fmt.Fprintf(&builder, "+%s: %s\n", project.ID, recommendation.PoolForProject(project.ID))
	}
	return strings.TrimRight(builder.String(), "\n")
}

func applyAgentPoolsFix(plan agentPoolsFixPlan) error {
	if plan.Noop || len(plan.After) == 0 {
		return nil
	}
	current, err := os.ReadFile(plan.Path)
	if err != nil {
		return fmt.Errorf("read global config %s before applying agent-pool fix: %w", plan.Path, err)
	}
	if !bytes.Equal(current, plan.Before) {
		return errors.New("global config changed since the agent-pool recommendation was prepared; rerun detent fix agent-pools")
	}
	if err := os.WriteFile(plan.Path, plan.After, configFileMode); err != nil {
		return fmt.Errorf("write global config %s: %w", plan.Path, err)
	}
	if err := os.Chmod(plan.Path, configFileMode); err != nil {
		return fmt.Errorf("restrict global config %s: %w", plan.Path, err)
	}
	return nil
}

func writeAgentPoolsFixResult(out io.Writer, result agentPoolsFixResult) error {
	if result.Diff != "" {
		if _, err := fmt.Fprintln(out, "Agent pool recommendation:"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, result.Diff); err != nil {
			return err
		}
	}
	if result.Message != "" {
		if _, err := fmt.Fprintln(out, result.Message); err != nil {
			return err
		}
	}
	switch {
	case result.Noop:
		return nil
	case result.DryRun:
		_, err := fmt.Fprintln(out, "Dry run; no files changed.")
		return err
	case result.Applied:
		return nil
	default:
		return nil
	}
}
