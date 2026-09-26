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
// A "fresh" workspace gets a worktree keyed by the work item and the workspace
// session, so it never holds the branch an ordinary run of the same issue
// needs, and two sessions on one issue never share a checkout.
//
// A "retained" workspace serves the worktree the attempt's run already has,
// found under the run's own identifier and never created, moved or removed
// here: the attempt's lifecycle owns it, including its branch.
type GitWorktree struct {
	// Backend is the runner's worktree backend.
	Backend workspace.Backend
	// ProjectID scopes the worktree key, as it does for a run.
	ProjectID string
	// Resolve maps a work item onto the identifier the runner's own runs key
	// their worktrees on. A retained workspace cannot be served without it.
	Resolve func(ctx context.Context, workItemID string) (string, error)
	mu      sync.Mutex
	// created records which paths this type made, so Release removes only
	// those: a retained worktree belongs to the attempt, not to us.
	created map[string]workspace.Issue
}

// existingBackend is the backend lookup a retained workspace needs.
type existingBackend interface {
	Existing(workspace.Issue) (workspace.Info, error)
}

// Prepare produces the worktree the hub asked for.
func (g *GitWorktree) Prepare(ctx context.Context, workspaceID string, checkout hubclient.WorkspaceCheckout) (string, error) {
	if g.Backend == nil {
		return "", errors.New("workspacerunner: a worktree backend is required")
	}
	if checkout.Worktree == workspacesession.WorktreeRetained {
		return g.retained(ctx, checkout)
	}
	if strings.TrimSpace(workspaceID) == "" {
		return "", errors.New("workspacerunner: a fresh workspace needs its session id")
	}
	head := strings.TrimSpace(checkout.HeadSHA)
	if head != "" && !commitID(head) {
		return "", fmt.Errorf("workspace head %q is not a commit id", head)
	}
	issue := workspace.Issue{
		ProjectID: g.ProjectID, ID: checkout.WorkItemID, Identifier: checkout.WorkItemID + "-" + workspaceID,
		BaseRef: checkout.Ref, PullRequestHeadSHA: head, WorkspaceSession: true,
	}
	info, err := g.Backend.Create(ctx, issue)
	if err != nil {
		return "", fmt.Errorf("create workspace worktree: %w", err)
	}
	if info.Created {
		g.mu.Lock()
		if g.created == nil {
			g.created = map[string]workspace.Issue{}
		}
		g.created[info.Path] = issue
		g.mu.Unlock()
	}
	if head != "" {
		// head_sha is what the person asked to look at. Detaching onto it is
		// deliberate: a workspace produces nothing, so there is no branch for
		// it to be on.
		if err := g.checkout(ctx, info.Path, head); err != nil {
			return "", errors.Join(err, g.Release(ctx, info.Path, checkout))
		}
	}
	return info.Path, nil
}

// retained finds the attempt's own worktree. The request's ref is ignored:
// the worktree is served on whatever branch the attempt's run put it.
func (g *GitWorktree) retained(ctx context.Context, checkout hubclient.WorkspaceCheckout) (string, error) {
	existing, ok := g.Backend.(existingBackend)
	if !ok || g.Resolve == nil {
		return "", errors.New("workspacerunner: this runner cannot locate a retained attempt worktree")
	}
	identifier, err := g.Resolve(ctx, checkout.WorkItemID)
	if err != nil {
		return "", fmt.Errorf("resolve retained worktree for %s: %w", checkout.WorkItemID, err)
	}
	info, err := existing.Existing(workspace.Issue{ProjectID: g.ProjectID, ID: checkout.WorkItemID, Identifier: identifier})
	if err != nil {
		return "", fmt.Errorf("find retained worktree for %s: %w", identifier, err)
	}
	return info.Path, nil
}

// commitID reports whether value is a hexadecimal object name, which is the
// only shape the hub sends as head_sha and the only one handed to git.
func commitID(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
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

// checkout detaches the worktree onto a commit.
func (g *GitWorktree) checkout(ctx context.Context, path, sha string) error {
	command := exec.CommandContext(ctx, "git", "-C", path, "checkout", "--detach", sha) // #nosec G204 -- sha is a validated hex commit id and git runs without a shell.
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("check out %s in the workspace worktree: %w: %s", sha, err, strings.TrimSpace(string(output)))
	}
	return nil
}
