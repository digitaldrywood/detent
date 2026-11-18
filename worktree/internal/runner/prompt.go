package runner

import (
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner/prompt"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const DefaultPromptTemplate = prompt.DefaultPromptTemplate
const routineToolInstructions = prompt.RoutineToolInstructions
const admissionToolInstructions = prompt.AdmissionToolInstructions
const repositoryHandoffContract = prompt.RepositoryHandoffContract

type PromptOptions = prompt.PromptOptions

type ValidatorPromptOptions struct {
	SecurityAudit        *NativeSecurityAuditEvidence
	Discussion           NativeReviewDiscussion
	Evidence             []tracker.NativeEvidence
	implementationReview bool
	Commands             []gate.CommandResult
	VersionID            string
	WorkspacePath        string
	Branch               string
	Repository           string
	PRNumber             int
	BaseSHA              string
	HeadSHA              string
	DiffDigest           string
	DiffFiles            []string
	DiffStat             *workspace.DiffStat
	DiffPatch            string
	DiffTruncated        bool
	DiffError            string
	MaxInlineDiffBytes   int
}

func BuildPrompt(workflow config.Workflow, issue connector.Issue, opts PromptOptions) (string, error) {
	return prompt.BuildPrompt(workflow, issue, opts)
}

func BuildRoutinePrompt(workflow config.Workflow, issue connector.Issue, routine RoutineRequest, opts PromptOptions) (string, error) {
	return prompt.BuildRoutinePrompt(workflow, issue, routine, opts)
}

func BuildAdmissionPrompt(issue connector.Issue, request AdmissionRequest, opts PromptOptions) (string, error) {
	return prompt.BuildAdmissionPrompt(issue, request, opts)
}

func BuildMergeFallbackPrompt(workflow config.Workflow, issue connector.Issue, opts PromptOptions) (string, error) {
	return prompt.BuildMergeFallbackPrompt(workflow, issue, opts)
}

func appendBlockedHandoffBlock(text string, opts PromptOptions, native bool) string {
	return prompt.AppendBlockedHandoffBlock(text, opts, native)
}

func appendNativeIssueInstructions(text string, issue connector.Issue) string {
	return prompt.AppendNativeIssueInstructions(text, issue)
}

func appendFollowupsBlock(text string, cfg config.Followups) string {
	return prompt.AppendFollowupsBlock(text, cfg)
}

func appendNativeCompletionContract(text string) string {
	return prompt.AppendNativeCompletionContract(text)
}

func githubIssueNumber(identifier string) string {
	return prompt.GitHubIssueNumber(identifier)
}

func validatorMaxInlineDiffBytes(cfg gate.ValidatorConfig) int {
	return prompt.ValidatorMaxInlineDiffBytes(cfg)
}

func BuildValidatorPrompt(workflow config.Workflow, issue connector.Issue, opts ValidatorPromptOptions) string {
	return prompt.BuildValidatorPrompt(workflow, issue, prompt.ValidatorPromptOptions{
		SecurityAudit:        opts.SecurityAudit,
		Discussion:           prompt.NativeReviewDiscussion(opts.Discussion),
		Evidence:             opts.Evidence,
		ImplementationReview: opts.implementationReview,
		Commands:             opts.Commands,
		VersionID:            opts.VersionID,
		WorkspacePath:        opts.WorkspacePath,
		Branch:               opts.Branch,
		Repository:           opts.Repository,
		PRNumber:             opts.PRNumber,
		BaseSHA:              opts.BaseSHA,
		HeadSHA:              opts.HeadSHA,
		DiffDigest:           opts.DiffDigest,
		DiffFiles:            opts.DiffFiles,
		DiffStat:             opts.DiffStat,
		DiffPatch:            opts.DiffPatch,
		DiffTruncated:        opts.DiffTruncated,
		DiffError:            opts.DiffError,
		MaxInlineDiffBytes:   opts.MaxInlineDiffBytes,
	})
}
