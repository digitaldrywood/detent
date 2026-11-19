package cli

import (
	"context"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/cli/doctor"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func doctorProjectID(project globalconfig.Project) string { return doctor.ProjectID(project) }

func projectSourceRoot(project globalconfig.Project, cfg workflowconfig.Config) string {
	return doctor.ProjectSourceRoot(project, cfg)
}

func expandDoctorWorkspacePath(path string) (string, error) {
	return doctor.ExpandWorkspacePath(path)
}

func resolveDoctorProjectPath(project globalconfig.Project, path string) (string, error) {
	return doctor.ResolveProjectPath(project, path)
}

const doctorWorkflowRuleRunawaySessionTokens = doctor.WorkflowRuleRunawaySessionTokens

const doctorWorkflowRulePinnedRouteModelRejected = doctor.WorkflowRulePinnedRouteModelRejected

type doctorWorkflowOptimizationReport = doctor.WorkflowOptimizationReport

type doctorWorkflowOptimizationFinding = doctor.WorkflowOptimizationFinding
type doctorWorkflowOptimizationPatch = doctor.WorkflowOptimizationPatch

func doctorReviewFlowWorkflowFindings(projectID string, workflowPath string, cfg workflowconfig.Config, prompt string) []doctorWorkflowOptimizationFinding {
	return doctor.ReviewFlowWorkflowFindings(projectID, workflowPath, cfg, prompt)
}

func doctorWorkflowWorkerModelChoice(cfg workflowconfig.Config) doctorWorkflowModelChoice {
	return doctor.WorkflowWorkerModelChoice(cfg)
}

func doctorWorkflowBackendConfigsByID(cfg workflowconfig.Config) map[string]workflowconfig.AgentBackend {
	return doctor.WorkflowBackendConfigsByID(cfg)
}

func doctorWorkflowBackendCommandModel(backend workflowconfig.AgentBackend) string {
	return doctor.WorkflowBackendCommandModel(backend)
}

func doctorWorkflowCommandFields(command string) []string {
	return doctor.WorkflowCommandFields(command)
}

func doctorWorkflowOptimizationWorkflowPath(project globalconfig.Project) (string, error) {
	return doctor.WorkflowOptimizationWorkflowPath(project)
}

func writeDoctorWorkflowOptimizationPretty(out io.Writer, report doctorWorkflowOptimizationReport) error {
	return doctor.WriteDoctorWorkflowOptimizationPretty(out, report)
}

func writeDoctorWorkflowOptimizationPatches(report doctorWorkflowOptimizationReport) ([]string, error) {
	return doctor.WriteDoctorWorkflowOptimizationPatches(report)
}

func doctorWorkflowOptimizationPatchesByWorkflow(report doctorWorkflowOptimizationReport) map[string][]doctorWorkflowOptimizationPatch {
	return doctor.WorkflowOptimizationPatchesByWorkflow(report)
}

func doctorWorkflowOptimizationDiff(report doctorWorkflowOptimizationReport) string {
	return doctor.WorkflowOptimizationDiff(report)
}

func doctorReviewFlowChoice(cfg workflowconfig.Config) string { return doctor.ReviewFlowChoice(cfg) }

func doctorReviewFlowGateWaitState(cfg workflowconfig.Config) string {
	return doctor.ReviewFlowGateWaitState(cfg)
}
func nativeWorkflowGitHubSteps(cfg workflowconfig.Config, prompt string) []string {
	return doctor.NativeWorkflowGitHubSteps(cfg, prompt)
}
func nativeWorkflowGitHubStepsWarning(steps []string) string {
	return doctor.NativeWorkflowGitHubStepsWarning(steps)
}

func checkDoctorNativeWorkflowInstructions(id string, cfg workflowconfig.Config, prompt string) doctorCheck {
	return doctorCheckResultFrom(doctor.CheckNativeWorkflowInstructions(id, cfg, prompt))
}

type doctorShipSkill = doctor.ShipSkill

func checkDoctorWorkflowLint(ctx context.Context, projectID string, project globalconfig.Project, cfg workflowconfig.Config, prompt string, workflowTokenThreshold int, deps doctorDeps) []doctorCheck {
	return doctorCheckResultsFrom(doctor.CheckWorkflowLint(ctx, projectID, project, cfg, prompt, workflowTokenThreshold, doctorCheckDeps(deps)))
}

func doctorRequiredCheckMatchesJob(required string, jobID string, jobName string) bool {
	return doctor.RequiredCheckMatchesJob(required, jobID, jobName)
}

func doctorYAMLMapValue(node *yaml.Node, key string) *yaml.Node {
	return doctor.YAMLMapValue(node, key)
}

func doctorYAMLScalarValue(node *yaml.Node) string { return doctor.YAMLScalarValue(node) }

func probeDoctorShipSkill(codexHome string) (doctorShipSkill, error) {
	return doctor.ProbeShipSkill(codexHome)
}

type doctorGitWorktree = doctor.GitWorktree

func checkDoctorWorkspaceGrowth(
	ctx context.Context,
	projectID string,
	workspaceRoot string,
	sourceRoot string,
	deps doctorDeps,
) (doctorCheck, bool) {
	check, ok := doctor.CheckWorkspaceGrowth(ctx, projectID, workspaceRoot, sourceRoot, doctorCheckDeps(deps))
	return doctorCheckResultFrom(check), ok
}

func checkDoctorExternalBranchWorktrees(
	ctx context.Context,
	projectID string,
	sourceRoot string,
	workspaceRoot string,
	deps doctorDeps,
) doctorCheck {
	return doctorCheckResultFrom(doctor.CheckExternalBranchWorktrees(ctx, projectID, sourceRoot, workspaceRoot, doctorCheckDeps(deps)))
}

func defaultDoctorGitWorktrees(ctx context.Context, sourceRoot string) ([]doctorGitWorktree, error) {
	return doctor.DefaultGitWorktrees(ctx, sourceRoot)
}

func uniqueDoctorStrings(values []string) []string { return doctor.UniqueStrings(values) }

func doctorTrackerUsesGitHubReads(kind string) bool { return doctor.TrackerUsesGitHubReads(kind) }

type doctorWorkflowImprovementProposal = doctor.WorkflowImprovementProposal

type doctorWorkflowProposalConnector = doctor.WorkflowProposalConnector

func doctorWorkflowProposalsForFindings(projectID string, findings []doctorWorkflowOptimizationFinding, threshold int) []doctorWorkflowImprovementProposal {
	return doctor.WorkflowProposalsForFindings(projectID, findings, threshold)
}

func doctorNormalizedWorkflowPolicy(raw []byte) []byte { return doctor.NormalizedWorkflowPolicy(raw) }

func doctorWorkflowSourceGit(ctx context.Context, sourceRoot string, args ...string) (string, error) {
	return doctor.WorkflowSourceGit(ctx, sourceRoot, args...)
}

func doctorCommandEnvironment(base []string, overrides []string) []string {
	return doctor.CommandEnvironment(base, overrides)
}

type doctorInstructionFile = doctor.InstructionFile

func doctorInstructionFiles(ctx context.Context, root, prompt, workflowPath string) ([]doctorInstructionFile, []string) {
	return doctor.InstructionFiles(ctx, root, prompt, workflowPath)
}

func checkDoctorInstructionBudget(id string, files []doctorInstructionFile, problems []string) doctorCheck {
	return doctorCheckResultFrom(doctor.CheckInstructionBudget(id, files, problems))
}
func checkDoctorGateInstructionConflict(id, command string, files []doctorInstructionFile, problems []string) doctorCheck {
	return doctorCheckResultFrom(doctor.CheckGateInstructionConflict(id, command, files, problems))
}
func checkDoctorWorkflowSourceDrift(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	return doctorCheckResultFrom(doctor.CheckWorkflowSourceDrift(ctx, id, project, cfg, doctorCheckDeps(deps)))
}

func checkDoctorModelPolicy(id string, cfg workflowconfig.Config) doctorCheck {
	return doctorCheckResultFrom(doctor.CheckModelPolicy(id, cfg))
}

type doctorCapabilityState = doctor.CapabilityState

const doctorCapabilityConfigured = doctor.CapabilityConfigured
const doctorCapabilityUnused = doctor.CapabilityUnused

type doctorCapabilityReport = doctor.CapabilityReport

func checkDoctorCapabilities(project globalconfig.Project, workflow workflowconfig.Workflow) doctorCheck {
	return doctorCheckResultFrom(doctor.CheckCapabilities(project, workflow))
}
func findDoctorIssueEffortGuidance(sourceRoot string) (_ string, resultErr error) {
	return doctor.FindIssueEffortGuidance(sourceRoot)
}

const doctorWorkflowDefaultTokenThreshold = doctor.WorkflowDefaultTokenThreshold

type doctorWorkflowSkillSuggestion = doctor.WorkflowSkillSuggestion

func doctorCheckDeps(deps doctorDeps) doctor.Deps {
	return doctor.Deps{
		LookupEnv:            deps.lookupEnv,
		ResolveCommandInDir:  deps.resolveCommandInDir,
		RunCommandInDir:      deps.runCommandInDir,
		ShipSkillProbe:       deps.shipSkillProbe,
		GitHubRepositoryInfo: deps.githubRepositoryInfo,
		GitWorktrees:         deps.gitWorktrees,
	}
}

func doctorCheckResultFrom(check doctor.Check) doctorCheck {
	return doctorCheck{
		Name:                     check.Name,
		Status:                   check.Status,
		Detail:                   check.Detail,
		Hint:                     check.Hint,
		WorkflowSkillSuggestions: check.WorkflowSkillSuggestions,
		Capabilities:             check.Capabilities,
		WorkflowOptimization:     check.WorkflowOptimization,
	}
}

func doctorCheckResultsFrom(checks []doctor.Check) []doctorCheck {
	if checks == nil {
		return nil
	}
	out := make([]doctorCheck, len(checks))
	for i, check := range checks {
		out[i] = doctorCheckResultFrom(check)
	}
	return out
}

const doctorWorkflowRuleRepositoryMergePolicy = doctor.WorkflowRuleRepositoryMergePolicy

type doctorWorkflowModelChoice = doctor.WorkflowModelChoice
