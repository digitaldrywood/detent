package phasegit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

type Command func(context.Context, ...string) (string, error)
type RemoteHead func(context.Context, string, string, string) (string, bool, error)

func Head(ctx context.Context, path string) (string, string, error) {
	head, err := gitexec.Run(ctx, path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", "", err
	}
	head = strings.TrimSpace(head)
	tree, err := gitexec.Run(ctx, path, "rev-parse", head+"^{tree}")
	if err != nil {
		return "", "", fmt.Errorf("read phase tree: %w", err)
	}
	return head, strings.TrimSpace(tree), nil
}

func VerifyCheckout(ctx context.Context, path string, receipt tracker.NativePhaseReceipt) error {
	head, tree, err := Head(ctx, path)
	if err != nil {
		return err
	}
	if head != receipt.HeadSHA || tree != receipt.TreeSHA {
		return errors.New("replacement worktree differs from the recorded phase source")
	}
	return nil
}

func Push(ctx context.Context, path, branch string, verify func(context.Context) error, authorize func(context.Context) error, remoteHead RemoteHead, pushError func(error, string) error) (string, string, string, error) {
	if branch == "" {
		return "", "", "", errors.New("phase publication requires an item branch")
	}
	ref := "refs/heads/" + branch
	if _, err := gitexec.Run(ctx, path, "check-ref-format", ref); err != nil {
		return "", "", "", fmt.Errorf("validate phase publication branch: %w", err)
	}
	if err := verify(ctx); err != nil {
		return "", "", "", err
	}
	head, tree, err := Head(ctx, path)
	if err != nil {
		return "", "", "", err
	}
	if err := authorize(ctx); err != nil {
		return "", "", "", err
	}
	if _, pushErr := gitexec.Run(ctx, path, "push", "--no-follow-tags", gitexec.DefaultRemote, head+":"+ref); pushErr != nil {
		current, exists, readErr := remoteHead(ctx, path, gitexec.DefaultRemote, branch)
		if readErr != nil || !exists || current != head {
			return "", "", "", errors.Join(pushError(pushErr, branch), readErr)
		}
	}
	return branch, head, tree, nil
}

func PreserveHistory(ctx context.Context, path, identifier string, previous tracker.NativePhaseReceipt, fetch, authorize func(context.Context) error, commit Command, remoteHead RemoteHead, moved error) error {
	head, tree, err := Head(ctx, path)
	if err != nil || head == previous.HeadSHA {
		return err
	}
	if err := fetch(ctx); err != nil {
		return err
	}
	if _, err := gitexec.Run(ctx, path, "merge-base", "--is-ancestor", previous.HeadSHA, head); err == nil {
		return nil
	} else {
		var commandErr *gitexec.CommandError
		if !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
			return err
		}
	}
	remote, exists, err := remoteHead(ctx, path, gitexec.DefaultRemote, previous.Branch)
	if err != nil {
		return err
	}
	if !exists || remote != previous.HeadSHA {
		return moved
	}
	if err := authorize(ctx); err != nil {
		return err
	}
	if _, err := commit(ctx, "-c", "core.hooksPath="+os.DevNull, "merge", "--no-ff", "-s", "ours", "-m", "fix: retain phase history for "+identifier, previous.HeadSHA); err != nil {
		return err
	}
	_, observedTree, err := Head(ctx, path)
	if err == nil && observedTree != tree {
		return errors.New("preserving the item branch changed the completed source tree")
	}
	return err
}

func Fetch(ctx context.Context, branch string, receipt tracker.NativePhaseReceipt, repository func(context.Context) string, expectedRepository string, acquire func(context.Context) (func(), error), git Command) error {
	if !gitexec.ValidHead(receipt.HeadSHA) || !gitexec.ValidHead(receipt.TreeSHA) || receipt.Branch == "" || receipt.Branch != branch {
		return errors.New("recorded phase does not name the item branch and immutable source")
	}
	repo := repository(ctx)
	expected := strings.TrimSpace(expectedRepository)
	if repo == "" || expected != "" && repo != expected {
		return errors.New("recorded phase source is outside the configured repository")
	}
	release, err := acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := git(ctx, "fetch", "--no-tags", gitexec.DefaultRemote, receipt.HeadSHA); err != nil {
		return fmt.Errorf("fetch recorded phase commit: %w", err)
	}
	head, err := git(ctx, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil || strings.TrimSpace(head) != receipt.HeadSHA {
		return errors.Join(errors.New("fetched commit differs from the recorded phase source"), err)
	}
	tree, err := git(ctx, "rev-parse", receipt.HeadSHA+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != receipt.TreeSHA {
		return errors.Join(errors.New("fetched tree differs from the recorded phase source"), err)
	}
	return nil
}
