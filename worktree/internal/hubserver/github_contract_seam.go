package hubserver

import "github.com/digitaldrywood/detent/internal/hubserver/githubcontract"

type MutationKind = githubcontract.MutationKind

type WorkflowLabelDesired = githubcontract.WorkflowLabelDesired

type WorkpadDesired = githubcontract.WorkpadDesired

type MergePullRequestDesired = githubcontract.MergePullRequestDesired

type OutboxItem = githubcontract.OutboxItem

type OutboxProgress = githubcontract.OutboxProgress

type OutboxObservation = githubcontract.OutboxObservation

type OutboxStepResult = githubcontract.OutboxStepResult

type OutboxStepBackend = githubcontract.OutboxStepBackend

type OutboxBackend = githubcontract.OutboxBackend

const MutationWorkflowLabel = githubcontract.MutationWorkflowLabel

const MutationWorkpad = githubcontract.MutationWorkpad

const MutationMergePullRequest = githubcontract.MutationMergePullRequest

var ErrOutboxUncertain = githubcontract.ErrOutboxUncertain

func Permanent(err error) error { return githubcontract.Permanent(err) }

type ReconcileMode = githubcontract.ReconcileMode

type RepositoryTarget = githubcontract.RepositoryTarget

type ReconcileRequest = githubcontract.ReconcileRequest

type HydrationRequest = githubcontract.HydrationRequest

type RepositorySource = githubcontract.RepositorySource

type IssueSource = githubcontract.IssueSource

type PullRequestSource = githubcontract.PullRequestSource

type PullRequestDetailSource = githubcontract.PullRequestDetailSource

type ReconcileSnapshot = githubcontract.ReconcileSnapshot

type ReconcileBackend = githubcontract.ReconcileBackend

const ReconcileIncremental = githubcontract.ReconcileIncremental

const ReconcileFullRepair = githubcontract.ReconcileFullRepair

type GitHubImportRequest = githubcontract.GitHubImportRequest

type GitHubImportRecord = githubcontract.GitHubImportRecord

type GitHubImportPage = githubcontract.GitHubImportPage

type ImportBackend = githubcontract.ImportBackend

type GitHubAppInstallation = githubcontract.GitHubAppInstallation

type GitHubRequestCount = githubcontract.GitHubRequestCount

type ReleasePickVerifier = githubcontract.ReleasePickVerifier

type ReleaseDeliveryVerifier = githubcontract.ReleaseDeliveryVerifier

type ReleasePickPublisher = githubcontract.ReleasePickPublisher

var ErrReleasePickMismatch = githubcontract.ErrReleasePickMismatch
