package project

import (
	"context"
	"net/url"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func repositoryPolicySource(ctx context.Context, cfg globalconfig.Project, workflow workflowconfig.Workflow) (workflowconfig.Workflow, *policy.RepositorySource) {
	if workflow.Definition.Layout != workflowconfig.ProjectDefinitionSplit && workflow.Definition.Layout != workflowconfig.ProjectDefinitionLegacy {
		return workflow, nil
	}
	repository := EffectivePolicyConfig(cfg, workflow.Config).Tracker.Repository
	if repository == "" {
		return workflow, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	origin, err := runWorkflowGit(ctx, cfg.Workdir, "config", "--get", "remote.origin.url")
	if err != nil {
		return workflow, nil
	}
	originURL, err := url.Parse(workspace.HTTPSRemoteURL(string(origin)))
	if err != nil || originURL.Host == "" || !strings.EqualFold(strings.TrimPrefix(originURL.Path, "/"), repository) {
		return workflow, nil
	}
	sourceConfig := cfg
	if sourceConfig.WorkflowRef == "" {
		sourceConfig.WorkflowRef = "HEAD"
	}
	source, err := newWorkflowGitRefSource(sourceConfig)
	if err != nil {
		return workflow, nil
	}
	committed, revision, err := source.loadSource(ctx, false)
	if err != nil || workflow.SourceHash != committed.SourceHash {
		return workflow, nil
	}
	candidate := workflow
	candidate.Definition.Revision = revision
	committed.Config = MapNativeTracker(committed.Config, true)
	localPolicy, err := ResolvePolicy(cfg, candidate)
	if err != nil {
		return workflow, nil
	}
	committedPolicy, err := ResolvePolicy(cfg, committed)
	if err != nil {
		return workflow, nil
	}
	if committedPolicy.Workflow != nil && localPolicy.Workflow != nil {
		committedPolicy.Workflow.Source = localPolicy.Workflow.Source
		committedPolicy = committedPolicy.WithID()
	}
	if localPolicy.Match(committedPolicy) != nil {
		return workflow, nil
	}
	provenance := &policy.RepositorySource{Repository: repository, Commit: revision}
	remote, err := runWorkflowGit(ctx, cfg.Workdir, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return workflow, nil
	}
	for line := range strings.SplitSeq(string(remote), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
			provenance.DefaultBranch, _ = strings.CutPrefix(fields[1], "refs/heads/")
		}
		if len(fields) == 2 && fields[1] == "HEAD" {
			provenance.DefaultBranchHead = fields[0]
		}
	}
	if provenance.DefaultBranch == "" || provenance.DefaultBranchHead == "" {
		return workflow, nil
	}
	if _, err := runWorkflowGit(ctx, cfg.Workdir, "merge-base", "--is-ancestor", revision, provenance.DefaultBranchHead); err == nil {
		provenance.DefaultBranchReachable = true
	}
	return candidate, provenance
}
