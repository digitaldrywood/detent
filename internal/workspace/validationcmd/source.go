package validationcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

func RunReview(ctx context.Context, path, expected, command string, run func(context.Context, string) (gate.CommandResult, error)) (gate.CommandResult, error) {
	head, err := gitexec.Run(ctx, path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return gate.CommandResult{}, err
	}
	if strings.TrimSpace(head) != expected {
		return gate.CommandResult{}, errors.New("review command head differs from the reviewed version")
	}
	if err := VerifyCleanReview(ctx, path); err != nil {
		return gate.CommandResult{}, err
	}
	tree, err := gitexec.Run(ctx, path, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return gate.CommandResult{}, err
	}
	result, err := run(ctx, command)
	result.HeadSHA = strings.TrimSpace(head)
	result.TreeSHA = strings.TrimSpace(tree)
	result.ReceiptID = result.PipelineTiming("").ReceiptID
	const maxOutputBytes = 64 * 1024
	output, truncated := runtimeoutput.Diagnostic(result.Output, maxOutputBytes)
	result.Output = output
	result.OutputTruncated = result.OutputTruncated || truncated
	if err != nil {
		return result, err
	}
	current, err := gitexec.Run(ctx, path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(current) != result.HeadSHA {
		return result, errors.New("review command changed the reviewed head")
	}
	return result, VerifyCleanReview(ctx, path)
}

func RunBase(ctx context.Context, source, base string, prepare func() (string, error), run func(string) (gate.CommandResult, error)) (_ gate.CommandResult, err error) {
	if !changerequest.ValidHash(base, 40) && !changerequest.ValidHash(base, 64) {
		return gate.CommandResult{}, errors.New("base reproduction requires an exact commit")
	}
	scratch, err := prepare()
	if err != nil {
		return gate.CommandResult{}, err
	}
	removeScratch := true
	defer func() {
		if removeScratch {
			if cleanupErr := os.RemoveAll(scratch); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove base reproduction scratch: %w", cleanupErr))
			}
		}
	}()
	path := filepath.Join(scratch, "base")
	if _, err := gitexec.Run(ctx, source, "clone", "--shared", "--no-checkout", "--no-hardlinks", "--", source, path); err != nil {
		return gate.CommandResult{}, fmt.Errorf("clone base reproduction source: %w", err)
	}
	if _, err := gitexec.Run(ctx, path, "checkout", "--detach", base); err != nil {
		return gate.CommandResult{}, fmt.Errorf("checkout reproduction base: %w", err)
	}
	result, err := run(path)
	removeScratch = err == nil
	result.HeadSHA = base
	if err == nil {
		current, headErr := gitexec.Run(ctx, path, "rev-parse", "HEAD")
		status, treeErr := gitexec.Run(ctx, path, "status", "--porcelain")
		if headErr != nil || treeErr != nil || strings.TrimSpace(current) != base || strings.TrimSpace(status) != "" {
			err = errors.New("base reproduction changed source")
		}
	}
	output, truncated := runtimeoutput.Diagnostic(result.Output, 64*1024)
	result.Output, result.OutputTruncated = output, truncated
	return result, err
}

func VerifyCleanReview(ctx context.Context, path string) error {
	changes, err := gitexec.Run(ctx, path, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all")
	if err != nil {
		return err
	}
	if changes != "" {
		var paths []string
		for entry := range strings.SplitSeq(changes, "\x00") {
			if len(entry) > 3 {
				paths = append(paths, entry[3:])
			}
		}
		slices.Sort(paths)
		return fmt.Errorf("review workspace has local changes: %q", paths)
	}
	return nil
}

func PrepareIsolatedSource(ctx context.Context, source, scratch, head string) (string, error) {
	path := filepath.Join(scratch, "source")
	if _, err := gitexec.Run(ctx, source, "clone", "--shared", "--no-checkout", "--", source, path); err != nil {
		return "", fmt.Errorf("prepare validation source: %w", err)
	}
	if _, err := gitexec.Run(ctx, path, "remote", "remove", "origin"); err != nil {
		return "", fmt.Errorf("remove validation source remote: %w", err)
	}
	refs, err := gitexec.Run(ctx, source, "for-each-ref", "--format=update %(refname) %(objectname)", "refs/remotes/")
	if err != nil {
		return "", fmt.Errorf("read validation base refs: %w", err)
	}
	if refs != "" {
		if _, err := gitexec.WithInput(ctx, path, nil, true, refs, "update-ref", "--stdin"); err != nil {
			return "", fmt.Errorf("copy validation base refs: %w", err)
		}
	}
	if _, err := gitexec.Run(ctx, path, "checkout", "--detach", head); err != nil {
		return "", fmt.Errorf("checkout validation source: %w", err)
	}
	return path, nil
}

func IsolatedSourceUnchanged(ctx context.Context, path, expected string) (bool, error) {
	head, headErr := gitexec.Run(ctx, path, "rev-parse", "HEAD")
	status, statusErr := gitexec.Run(ctx, path, "status", "--porcelain", "--untracked-files=no")
	if inspectionErr := errors.Join(headErr, statusErr); inspectionErr != nil {
		return false, fmt.Errorf("inspect validation source: %w", inspectionErr)
	}
	return strings.TrimSpace(head) == expected && status == "", nil
}
func AttributeCommandFailure(result *gate.CommandResult, base string, reproduction gate.CommandResult, err error) {
	if result.ExitCode == 0 {
		return
	}
	observation := gate.CommandObservation(reproduction)
	observation.Complete = observation.Complete && err == nil
	result.BaseReproduction = nil
	if reproduction.Command != "" {
		result.BaseReproduction = &observation
	}
	gate.NormalizeCommandAttribution(base, result.HeadSHA, result)
}
