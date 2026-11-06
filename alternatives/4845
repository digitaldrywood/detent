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
	// holds releases each served path's workspace.HoldSession, which keeps
	// cleanup and residual reconciliation off a worktree a session is using.
	holds map[string]func()
}

// hold marks path as served until Release.
func (g *GitWorktree) hold(path string) {
	release := workspace.HoldSession(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.holds == nil {
		g.holds = map[string]func(){}
	}
	if previous, ok := g.holds[path]; ok {
		previous()
	}
	g.holds[path] = release
}

// unhold ends the hold on path, if there is one.
func (g *GitWorktree) unhold(path string) {
	g.mu.Lock()
	release, ok := g.holds[path]
	delete(g.holds, path)
	g.mu.Unlock()
	if ok {
		release()
	}
}

// ProvidesGit reports whether the backend produces git worktrees, which is what
// the git channel needs. A filesystem backend produces plain directories.
func (g *GitWorktree) ProvidesGit() bool {
	_, ok := g.Backend.(*workspace.LocalGit)
	return ok
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
	ref := strings.TrimSpace(checkout.Ref)
	if head == "" && ref != "" {
		if err := g.validRef(ctx, ref); err != nil {
			return "", err
		}
	}
	issue := workspace.Issue{
		ProjectID: g.ProjectID, ID: checkout.WorkItemID, Identifier: checkout.WorkItemID + "-" + workspaceID,
		BaseRef: ref, PullRequestHeadSHA: head, WorkspaceSession: true,
	}
	info, err := g.Backend.Create(ctx, issue)
	if err != nil {
		return "", fmt.Errorf("create workspace worktree: %w", err)
	}
	g.hold(info.Path)
	if info.Created {
		g.mu.Lock()
		if g.created == nil {
			g.created = map[string]workspace.Issue{}
		}
		g.created[info.Path] = issue
		g.mu.Unlock()
	}
	target := head
	switch {
	case head != "":
		err = g.ensureCommit(ctx, info.Path, head)
	case ref != "":
		target, err = g.resolveRef(ctx, info.Path, ref)
	}
	if err == nil && target != "" {
		// Detaching is deliberate: a workspace produces nothing, so there is
		// no branch for it to be on.
		err = g.checkout(ctx, info.Path, target)
	}
	if err != nil {
		// Nothing was checked out, so the worktree is still on its own session
		// branch and is judged without a head the checkout never reached.
		g.recordHead(info.Path, "")
		return "", errors.Join(err, g.Release(ctx, info.Path, checkout))
	}
	g.recordHead(info.Path, target)
	return info.Path, nil
}

// recordHead records the commit a created worktree was opened on, which is
// what its cleanup measures new work against.
func (g *GitWorktree) recordHead(path, head string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if created, ok := g.created[path]; ok {
		created.PullRequestHeadSHA = head
		g.created[path] = created
	}
}

// validRef refuses a ref git would read as an option or would not accept as
// a ref name.
func (g *GitWorktree) validRef(ctx context.Context, ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("workspace ref %q is not a ref name", ref)
	}
	if _, err := runGit(ctx, "", "check-ref-format", "--allow-onelevel", ref); err != nil {
		return fmt.Errorf("workspace ref %q is not a ref name: %w", ref, err)
	}
	return nil
}

// ensureCommit fetches head from the worktree's remote when it is not already
// in the local object store.
func (g *GitWorktree) ensureCommit(ctx context.Context, path, head string) error {
	if _, err := runGit(ctx, path, "cat-file", "-e", head+"^{commit}"); err == nil {
		return nil
	}
	if _, err := runGit(ctx, path, "fetch", "--quiet", "--no-tags", "origin", "--end-of-options", head); err != nil {
		return fmt.Errorf("fetch workspace head %s: %w", head, err)
	}
	return nil
}

// resolveRef answers the commit a ref names, fetching it from the remote when
// no local ref resolves.
func (g *GitWorktree) resolveRef(ctx context.Context, path, ref string) (string, error) {
	if sha, err := runGit(ctx, path, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}"); err == nil {
		return sha, nil
	}
	if _, err := runGit(ctx, path, "fetch", "--quiet", "--no-tags", "origin", "--end-of-options", ref); err != nil {
		return "", fmt.Errorf("fetch workspace ref %s: %w", ref, err)
	}
	sha, err := runGit(ctx, path, "rev-parse", "--verify", "--quiet", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve fetched workspace ref %s: %w", ref, err)
	}
	return sha, nil
}

// runGit runs one git command, in dir when it is set, and answers its trimmed
// output.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	command := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- arguments are fixed subcommands and validated commit ids or ref names; git runs without a shell.
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[len(args)-1], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
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
	g.hold(info.Path)
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
	g.unhold(path)
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
	if _, err := runGit(ctx, path, "checkout", "--detach", sha); err != nil {
		return fmt.Errorf("check out %s in the workspace worktree: %w", sha, err)
	}
	return nil
}
