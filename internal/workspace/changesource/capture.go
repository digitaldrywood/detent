package changesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/checkpoint"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

type Source struct {
	Version       tracker.ChangeVersion       `json:"version"`
	Bundle        []byte                      `json:"bundle,omitempty"`
	PreviousHeads []string                    `json:"previous_heads,omitempty"`
	Transferred   bool                        `json:"transferred,omitempty"`
	PushedPhase   *tracker.NativePhaseReceipt `json:"pushed_phase,omitempty"`
}

func Diff(ctx context.Context, directory, base, head string) (digest string, err error) {
	err = gitexec.WithPublicationObjects(ctx, directory, func(scratch string) error {
		digest, err = diffDigest(ctx, scratch, base, head)
		return err
	})
	return digest, err
}

func diffDigest(ctx context.Context, scratch, base, head string) (string, error) {
	diff, truncated, err := gitexec.PublicationDiffWithinLimit(ctx, scratch, tracker.MaxChangeDiffBytes,
		"--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
	if err != nil {
		return "", err
	}
	if truncated {
		return "", errors.New("change source diff metadata exceeds the publication limit")
	}
	return tracker.ChangeSourceDigest([]byte(diff)), nil
}

func Capture(ctx context.Context, directory, base, head string) (capture tracker.ChangeSourceCapture, err error) {
	if !gitexec.ValidHead(base) || !gitexec.ValidHead(head) || base == head {
		return capture, errors.New("change source capture requires immutable base and head")
	}
	observed, err := gitexec.Run(ctx, directory, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(observed) != head {
		return capture, errors.Join(errors.New("change source capture differs from the finalized head"), err)
	}
	git := func(ctx context.Context, args ...string) (string, error) { return gitexec.Run(ctx, directory, args...) }
	if err := VerifyHistory(ctx, git, base, head); err != nil {
		return capture, err
	}
	objects, err := objects(ctx, directory, base, head)
	if err != nil {
		return capture, err
	}
	scratch, err := gitexec.Scratch()
	if err != nil {
		return capture, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	prefix := filepath.Join(scratch, "source")
	packHash, err := gitexec.WithInput(ctx, directory, nil, false, objects,
		"pack-objects", "--delta-base-offset", "--max-pack-size=0", prefix)
	if err != nil {
		return capture, err
	}
	packPath := prefix + "-" + strings.TrimSpace(packHash) + ".pack"
	header := fmt.Sprintf("# v2 git bundle\n-%s base\n%s HEAD\n\n", base, head)
	info, err := os.Stat(packPath)
	if err != nil {
		return capture, err
	}
	if info.Size()+int64(len(header)) > tracker.MaxChangeSourceBytes {
		return capture, errors.New("change source bundle exceeds the native publication limit")
	}
	pack, err := os.ReadFile(packPath)
	if err != nil {
		return capture, err
	}
	capture.Bundle = append([]byte(header), pack...)
	path := filepath.Join(scratch, "source.bundle")
	if err := os.WriteFile(path, capture.Bundle, 0o600); err != nil {
		return capture, err
	}
	description, err := Describe(ctx, directory, base, head)
	if err != nil {
		return capture, err
	}
	capture.Patch = description.Patch
	capture.Source = tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(capture.Bundle), DiffSHA256: description.Source.DiffSHA256, Bytes: int64(len(capture.Bundle))}
	if err := VerifyBundle(ctx, directory, path); err != nil {
		return capture, err
	}
	return capture, capture.Source.Validate(base, head, capture.Bundle)
}

func VerifyHistory(ctx context.Context, git checkpoint.GitRunner, base, head string) error {
	commits, err := git(ctx, "rev-list", base+".."+head)
	if err != nil {
		return err
	}
	var paths []string
	for _, commit := range strings.Fields(commits) {
		changed, err := git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "-r", "-m", "-z", commit)
		if err != nil {
			return err
		}
		for _, path := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if !checkpoint.PathAllowed(path) || strings.HasPrefix(strings.ToLower(path), ".detent/") {
			return fmt.Errorf("%w: excluded source path %q", checkpoint.ErrUnsafe, path)
		}
	}
	if err := checkpoint.History(ctx, git, base, head, paths); err != nil {
		return err
	}
	return nil
}

func Describe(ctx context.Context, directory, base, head string) (tracker.ChangeSourceCapture, error) {
	var capture tracker.ChangeSourceCapture
	if !gitexec.ValidHead(base) || !gitexec.ValidHead(head) || base == head {
		return capture, errors.New("change source description requires immutable base and head")
	}
	observed, err := gitexec.ReadPublicationSource(ctx, directory, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(observed) != head {
		return capture, errors.Join(errors.New("change source description differs from the finalized head"), err)
	}
	err = gitexec.WithPublicationObjects(ctx, directory, func(scratch string) error {
		if _, err := gitexec.ReadPublicationSource(ctx, scratch, "read-tree", head); err != nil {
			return fmt.Errorf("prepare review attributes from the finalized tree: %w", err)
		}
		patch, truncated, err := gitexec.PublicationDiffWithinLimit(ctx, scratch, tracker.MaxChangeDiffBytes,
			"--no-ext-diff", "--no-textconv", "--find-renames", base, head, "--")
		if err != nil {
			return err
		}
		if !truncated {
			capture.Patch = patch
		}
		digest, err := diffDigest(ctx, scratch, base, head)
		if err != nil {
			return err
		}
		capture.Source = tracker.ChangeSource{Format: tracker.ChangeSourceGitPhase, BaseSHA: base, HeadSHA: head, DiffSHA256: digest}
		return nil
	})
	return capture, err
}

func objects(ctx context.Context, directory, base, head string) (string, error) {
	objects, err := gitexec.Run(ctx, directory, "rev-list", "--objects", "--no-object-names", head, "^"+base)
	if err != nil {
		return "", err
	}
	types, err := gitexec.WithInput(ctx, directory, nil, false, objects, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return "", err
	}
	var retained strings.Builder
	retained.WriteString(objects)
	git := func(ctx context.Context, args ...string) (string, error) { return gitexec.Run(ctx, directory, args...) }
	for line := range strings.SplitSeq(strings.TrimSpace(types), "\n") {
		tree, kind, ok := strings.Cut(line, " ")
		if !ok || kind != "tree" {
			continue
		}
		attributes, err := git(ctx, "ls-tree", "--format=%(objectname)", tree, "--", ".gitattributes")
		if err != nil {
			return "", err
		}
		if attributes == "" {
			continue
		}
		if err := checkpoint.Blob(ctx, git, tree+":", ".gitattributes"); err != nil {
			return "", err
		}
		retained.WriteString(attributes)
	}
	return retained.String(), nil
}

func VerifyBundle(ctx context.Context, directory, path string) error {
	_, err := gitexec.Run(ctx, directory, "bundle", "verify", path)
	return err
}

func Verify(ctx context.Context, source tracker.ChangeSource, bundle []byte) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !gitexec.ValidHead(source.BaseSHA) || !gitexec.ValidHead(source.HeadSHA) {
		return errors.New("change source verification requires immutable base and head")
	}
	if err := source.Validate(source.BaseSHA, source.HeadSHA, bundle); err != nil {
		return err
	}
	_, pack, _ := bytes.Cut(bundle, []byte("\n\n"))
	if err := PackBudget(ctx, pack); err != nil {
		return err
	}
	scratch, err := gitexec.Scratch()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	if err := gitexec.InitScratch(ctx, scratch, "--bare", "--object-format=sha1"); err != nil {
		return err
	}
	git := func(input []byte, args ...string) error {
		command := exec.CommandContext(ctx, "git")
		command.Args = append([]string{"git"}, args...)
		command.Dir = scratch
		command.Env = gitexec.IsolatedEnvironment()
		command.Stdin = bytes.NewReader(input)
		command.WaitDelay = gitexec.WaitDelay
		procgroup.Configure(ctx, command)
		return commandshell.RunCommand(ctx, command)
	}
	if err := git(pack, "-c", "pack.threads=1", "-c", "core.deltaBaseCacheLimit=16m", "index-pack", "--stdin", "--fsck-objects"); err != nil {
		return errors.Join(ctx.Err(), errors.New("source bundle pack is invalid; recreate it with git bundle create <path> HEAD ^<base>"))
	}
	if err := git(nil, "cat-file", "-e", source.HeadSHA+"^{commit}"); err != nil {
		return errors.Join(ctx.Err(), errors.New("source bundle pack lacks the reviewed commit; recreate it with git bundle create <path> HEAD ^<base>"))
	}
	return nil
}
