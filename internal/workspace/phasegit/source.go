package phasegit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/internal/model"
	"github.com/digitaldrywood/detent/internal/workspace/validationcmd"
)

type Command func(context.Context, ...string) (string, error)

func Head(ctx context.Context, path string) (string, string, error) {
	return readHead(func(args ...string) (string, error) { return gitexec.ReadPublicationSource(ctx, path, args...) })
}

func readHead(read func(...string) (string, error)) (string, string, error) {
	head, err := read("rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", "", err
	}
	head = strings.TrimSpace(head)
	tree, err := read("rev-parse", head+"^{tree}")
	if err != nil {
		return "", "", fmt.Errorf("read phase tree: %w", err)
	}
	return head, strings.TrimSpace(tree), nil
}

func VerifyCheckout(ctx context.Context, path, head, tree string) error {
	return verifyCheckout(func(args ...string) (string, error) { return gitexec.ReadPublicationSource(ctx, path, args...) }, head, tree)
}

func VerifyCleanCheckout(ctx context.Context, path, owner, head, tree string) error {
	return gitexec.WithPublicationCheckoutFromHead(ctx, path, owner, func(read func(...string) (string, error)) error {
		if err := verifyCheckout(read, head, tree); err != nil {
			return err
		}
		return validationcmd.VerifyPublicationTreeFromHead(ctx, path, read)
	})
}

func verifyCheckout(read func(...string) (string, error), expectedHead, expectedTree string) error {
	head, tree, err := readHead(read)
	if err != nil {
		return err
	}
	if head != expectedHead || tree != expectedTree {
		return errors.New("replacement worktree differs from the verified phase source")
	}
	return nil
}

func Push(ctx context.Context, path, owner, branch, repository string, expectedRemoteHead *string, authorize, verifyDestination func(context.Context) error, verifyHistory func(gitexec.BoundedRead, string, string) error, moved func() error) (string, string, string, error) {
	if branch == "" {
		return "", "", "", errors.New("phase publication requires an item branch")
	}
	if err := authorize(ctx); err != nil {
		return "", "", "", err
	}
	if err := verifyDestination(ctx); err != nil {
		return "", "", "", err
	}
	ref := "refs/heads/" + branch
	var head, tree string
	err := gitexec.WithPublicationCheckout(ctx, path, owner, func(read func(...string) (string, error), readBounded gitexec.BoundedRead, push func(string, string, ...string) error) error {
		if _, err := read("check-ref-format", ref); err != nil {
			return fmt.Errorf("validate phase publication branch: %w", err)
		}
		if err := validationcmd.VerifyPublicationTree(ctx, path, read); err != nil {
			return err
		}
		var err error
		head, tree, err = readHead(read)
		if err != nil {
			return err
		}
		previous, _, err := gitexec.PublicationBranchHead(ctx, repository, branch)
		if err != nil {
			return err
		}
		expected := previous
		if expectedRemoteHead != nil {
			expected = *expectedRemoteHead
		}
		if previous != expected && previous != head {
			return moved()
		}
		if expected != "" && expected != head {
			if _, err := read("merge-base", "--is-ancestor", expected, head); err != nil {
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				return errors.Join(moved(), err)
			}
		}
		if previous == head {
			return authorize(ctx)
		}
		if err := verifyHistory(readBounded, head, previous); err != nil {
			return err
		}
		refspec, lease := head+":"+ref, "--force-with-lease="+ref+":"+expected
		var pushErr error
		for range 2 {
			if err := authorize(ctx); err != nil {
				return err
			}
			pushErr = push(repository, refspec, lease)
			if pushErr == nil {
				return nil
			}
			current, _, readErr := gitexec.PublicationBranchHead(ctx, repository, branch)
			if readErr != nil {
				return errors.Join(pushErr, readErr)
			}
			if current == head {
				return nil
			}
			if current != expected {
				return errors.Join(moved(), pushErr)
			}
		}
		return fmt.Errorf("push item phase to %s: %w", branch, pushErr)
	})
	if err != nil {
		return "", "", "", err
	}
	return branch, head, tree, nil
}

func PreserveHistory(ctx context.Context, path, identifier, repository string, previous model.PhaseSource, fetch, authorize func(context.Context) error, commit Command, moved error) (string, error) {
	head, tree, err := Head(ctx, path)
	if err != nil || head == previous.HeadSHA {
		return "", err
	}
	if err := fetch(ctx); err != nil {
		return "", err
	}
	if err := gitexec.VerifyPublicationAncestry(ctx, path, previous.HeadSHA, head); err == nil {
		return "", nil
	} else {
		var commandErr *gitexec.CommandError
		if !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
			return "", err
		}
	}
	remote, exists, err := gitexec.PublicationBranchHead(ctx, repository, previous.Branch)
	if err != nil {
		return "", err
	}
	if !exists || remote != previous.HeadSHA {
		return "", moved
	}
	if err := authorize(ctx); err != nil {
		return "", err
	}
	if _, err := commit(ctx, "-c", "core.hooksPath="+os.DevNull, "merge", "--no-ff", "-s", "ours", "-m", "fix: retain phase history for "+identifier, previous.HeadSHA); err != nil {
		return "", err
	}
	_, observedTree, err := Head(ctx, path)
	if err != nil {
		return "", err
	}
	if observedTree != tree {
		return "", errors.New("preserving the item branch changed the completed source tree")
	}
	return remote, nil
}

func Fetch(ctx context.Context, path, branch, repository string, source model.PhaseSource, acquire func(context.Context) (func(), error)) error {
	if !gitexec.ValidHead(source.HeadSHA) || !gitexec.ValidHead(source.TreeSHA) || source.Branch == "" || source.Branch != branch {
		return errors.New("recorded phase does not name the item branch and immutable source")
	}
	if repository == "" {
		return errors.New("recorded phase source is outside the configured repository")
	}
	release, err := acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	head, err := gitexec.FetchPhaseSource(ctx, path, repository, source.HeadSHA)
	if err != nil {
		return fmt.Errorf("fetch recorded phase commit: %w", err)
	}
	if head != source.HeadSHA {
		return errors.New("fetched commit differs from the recorded phase source")
	}
	tree, err := gitexec.ReadPublicationSource(ctx, path, "-c", "advice.graftFileDeprecated=false", "rev-parse", "--verify", source.HeadSHA+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != source.TreeSHA {
		return errors.Join(errors.New("fetched tree differs from the recorded phase source"), err)
	}
	return nil
}
