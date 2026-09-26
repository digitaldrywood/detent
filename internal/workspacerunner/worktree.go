package workspacerunner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// GitWorktree produces the checkout a workspace session serves, over the
// runner's ordinary git worktree backend.
//
// The important decision here is that a "fresh" workspace gets a worktree of
// its own rather than the issue's. The backend derives a worktree's path and
// branch from the issue identifier, so a workspace reusing that identifier
// would hold the branch an ordinary run of the same issue needs, and that run
// would fail with "branch held by worktree at ...". Section 18 does not say
// what should happen there; giving the workspace its own identifier means the
// question never arises, and a person opening Files on an issue never blocks
// the runner from working on it.
//
// A "retained" workspace is the opposite case and uses the issue's own
// identifier on purpose: the whole point is to show the worktree the attempt
// produced, and the attempt's own lifecycle still owns it. Such a worktree is
// never removed when the workspace closes.
type GitWorktree struct {
	// Backend is the runner's worktree backend.
	Backend workspace.Backend
	// ProjectID scopes the worktree key, as it does for a run.
	ProjectID string
	// Identify maps a work item onto the identifier the backend keys on. A
	// nil value uses the work item id, which is what the hub sends.
	Identify func(workItemID string) string
	mu       sync.Mutex
	// created records which paths this type made, so Release removes only
	// those: a retained worktree belongs to the attempt, not to us.
	created map[string]workspace.Issue
}

// Prepare produces the worktree the hub asked for.
func (g *GitWorktree) Prepare(ctx context.Context, checkout hubclient.WorkspaceCheckout) (string, error) {
	if g.Backend == nil {
		return "", errors.New("workspacerunner: a worktree backend is required")
	}
	issue := g.issueFor(checkout)
	info, err := g.Backend.Create(ctx, issue)
	if err != nil {
		return "", fmt.Errorf("create workspace worktree: %w", err)
	}
	if checkout.Worktree == workspacesession.WorktreeFresh && strings.TrimSpace(checkout.HeadSHA) != "" {
		// The retention window has passed, so the worktree is a new one and
		// head_sha is what the person asked to look at. Detaching onto it is
		// deliberate: a workspace produces nothing, so there is no branch for
		// it to be on.
		if err := g.checkout(ctx, info.Path, checkout.HeadSHA); err != nil {
			return "", err
		}
	}
	if info.Created {
		g.mu.Lock()
		if g.created == nil {
			g.created = map[string]workspace.Issue{}
		}
		g.created[info.Path] = issue
		g.mu.Unlock()
	}
	return info.Path, nil
}

// Release removes a worktree this type created and leaves a retained one
// alone. Closing a workspace never deletes the attempt's artifacts, and the
// attempt's worktree is the largest of them.
func (g *GitWorktree) Release(ctx context.Context, path string, checkout hubclient.WorkspaceCheckout) error {
	g.mu.Lock()
	issue, ours := g.created[path]
	delete(g.created, path)
	g.mu.Unlock()
	if !ours || checkout.Worktree == workspacesession.WorktreeRetained {
		return nil
	}
	cleaner, ok := g.Backend.(interface {
		CleanupIssue(context.Context, workspace.Issue) (workspace.CleanupResult, error)
	})
	if !ok {
		return g.Backend.Cleanup(ctx, issue.Identifier)
	}
	if _, err := cleaner.CleanupIssue(ctx, issue); err != nil {
		return fmt.Errorf("clean up workspace worktree: %w", err)
	}
	return nil
}

// issueFor builds the backend's view of what to check out.
func (g *GitWorktree) issueFor(checkout hubclient.WorkspaceCheckout) workspace.Issue {
	identifier := checkout.WorkItemID
	if g.Identify != nil {
		identifier = g.Identify(checkout.WorkItemID)
	}
	session := checkout.Worktree != workspacesession.WorktreeRetained
	if session {
		// A workspace of its own, so it cannot hold the branch an ordinary run
		// of the same issue needs.
		identifier = identifier + "-ws"
	}
	return workspace.Issue{
		ProjectID: g.ProjectID, ID: checkout.WorkItemID, Identifier: identifier,
		BranchName: checkout.Ref, BaseRef: checkout.Ref, PullRequestHeadSHA: checkout.HeadSHA,
		// The backend puts a session worktree in its own branch namespace, so
		// closing one removes the branch instead of retaining it as an
		// attempt's unpushed work.
		WorkspaceSession: session,
	}
}

// checkout detaches the worktree onto a commit.
func (g *GitWorktree) checkout(ctx context.Context, path, sha string) error {
	command := exec.CommandContext(ctx, "git", "-C", path, "checkout", "--detach", sha)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("check out %s in the workspace worktree: %w: %s", sha, err, strings.TrimSpace(string(output)))
	}
	return nil
}
