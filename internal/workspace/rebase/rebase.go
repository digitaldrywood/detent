package rebase

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/internal/workpath"
)

var ErrInvalidResolution = errors.New("merge resolution is invalid")

type State struct {
	Onto            string
	TargetBase      string
	ResolvedTarget  string
	ActiveCommit    string
	CanSkipEmpty    bool
	RescheduledPick bool
	PendingCommits  []string
}

func VerifyPaused(ctx context.Context, path string, resolveTarget func(context.Context) (string, string, error)) (State, error) {
	var onto string
	var rebaseKind string
	for _, kind := range []string{"rebase-merge", "rebase-apply"} {
		ontoPath, err := gitexec.PathFor(ctx, path, kind+"/onto")
		if err != nil {
			return State{}, err
		}
		raw, err := ReadRegularGitFile(ontoPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return State{}, err
		}
		onto = strings.TrimSpace(string(raw))
		rebaseKind = kind
		break
	}
	if onto == "" {
		return State{}, fmt.Errorf("%w: paused rebase integration base is unavailable", ErrInvalidResolution)
	}
	if !gitexec.ValidHead(onto) {
		return State{}, fmt.Errorf("%w: paused rebase integration base is invalid", ErrInvalidResolution)
	}
	base, target, err := resolveTarget(ctx)
	if err != nil {
		return State{}, err
	}
	if err := gitexec.VerifyPublicationAncestry(ctx, path, onto, base); err != nil {
		var commandErr *gitexec.CommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
			return State{}, fmt.Errorf("%w: paused rebase target is not on the selected integration branch", ErrInvalidResolution)
		}
		return State{}, fmt.Errorf("verify paused rebase integration base: %w", err)
	}
	head, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return State{}, err
	}
	head = strings.TrimSpace(head)
	if !gitexec.ValidHead(head) {
		return State{}, fmt.Errorf("%w: paused rebase head is invalid", ErrInvalidResolution)
	}
	headNamePath, err := gitexec.PathFor(ctx, path, rebaseKind+"/head-name")
	if err != nil {
		return State{}, err
	}
	rawHeadName, err := ReadRegularGitFile(headNamePath)
	if err != nil {
		return State{}, fmt.Errorf("%w: paused rebase branch metadata is unavailable", ErrInvalidResolution)
	}
	headName := strings.TrimSpace(string(rawHeadName))
	if !strings.HasPrefix(headName, "refs/heads/") {
		return State{}, fmt.Errorf("%w: paused rebase branch metadata is invalid", ErrInvalidResolution)
	}
	branchTip, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", headName+"^{commit}")
	if err != nil {
		return State{}, fmt.Errorf("%w: paused rebase branch tip is unavailable", ErrInvalidResolution)
	}
	branchTip = strings.TrimSpace(branchTip)
	origHeadPath, err := gitexec.PathFor(ctx, path, rebaseKind+"/orig-head")
	if err != nil {
		return State{}, err
	}
	rawOrigHead, err := ReadRegularGitFile(origHeadPath)
	if err != nil {
		return State{}, fmt.Errorf("%w: paused rebase original head is unavailable", ErrInvalidResolution)
	}
	origHead := strings.TrimSpace(string(rawOrigHead))
	if !gitexec.ValidHead(origHead) || origHead != branchTip {
		return State{}, fmt.Errorf("%w: paused rebase original head does not match assigned branch", ErrInvalidResolution)
	}
	expectedHead := onto
	maxReplayed := 0
	var accounted []string
	var stoppedCommit string
	var rescheduledPick bool
	var pendingCommits []string
	if rebaseKind == "rebase-merge" {
		rewrittenPath, err := gitexec.PathFor(ctx, path, "rebase-merge/rewritten-list")
		if err != nil {
			return State{}, err
		}
		rawRewritten, err := ReadRegularGitFile(rewrittenPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return State{}, err
		}
		rewritten := make(map[string]string)
		for line := range strings.SplitSeq(string(rawRewritten), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) < 2 || !gitexec.ValidHead(fields[0]) || !gitexec.ValidHead(fields[1]) {
				return State{}, fmt.Errorf("%w: paused rebase rewritten commit list is invalid", ErrInvalidResolution)
			}
			if _, dup := rewritten[fields[0]]; dup {
				return State{}, fmt.Errorf("%w: paused rebase duplicates a rewritten commit", ErrInvalidResolution)
			}
			if err := gitexec.VerifyPublicationAncestry(ctx, path, fields[0], origHead); err != nil {
				var commandErr *gitexec.CommandError
				if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
					return State{}, fmt.Errorf("%w: paused rebase replayed commit is not on the assigned branch", ErrInvalidResolution)
				}
				return State{}, fmt.Errorf("verify paused rebase source commit: %w", err)
			}
			rewritten[fields[0]] = fields[1]
		}
		stoppedPath, err := gitexec.PathFor(ctx, path, "rebase-merge/stopped-sha")
		if err != nil {
			return State{}, err
		}
		if rawStopped, sErr := ReadRegularGitFile(stoppedPath); sErr == nil {
			stopped := strings.TrimSpace(string(rawStopped))
			if stopped != "" {
				if !gitexec.ValidHead(stopped) {
					return State{}, fmt.Errorf("%w: paused rebase stopped commit is invalid", ErrInvalidResolution)
				}
				if err := gitexec.VerifyPublicationAncestry(ctx, path, stopped, origHead); err != nil {
					var commandErr *gitexec.CommandError
					if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
						return State{}, fmt.Errorf("%w: paused rebase stopped commit is not on the assigned branch", ErrInvalidResolution)
					}
					return State{}, fmt.Errorf("verify paused rebase stopped commit: %w", err)
				}
				stoppedCommit = stopped
			}
		} else if !errors.Is(sErr, os.ErrNotExist) {
			return State{}, sErr
		}
		donePath, err := gitexec.PathFor(ctx, path, "rebase-merge/done")
		if err != nil {
			return State{}, err
		}
		rawDone, doneErr := ReadRegularGitFile(donePath)
		if doneErr != nil && !errors.Is(doneErr, os.ErrNotExist) {
			return State{}, doneErr
		}
		if errors.Is(doneErr, os.ErrNotExist) && len(rewritten) > 0 {
			return State{}, fmt.Errorf("%w: paused rebase completed step log is unavailable", ErrInvalidResolution)
		}
		var doneSources []string
		for line := range strings.SplitSeq(string(rawDone), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 || (fields[0] != "pick" && fields[0] != "p") {
				return State{}, fmt.Errorf("%w: paused rebase completed log contains operations outside source replay", ErrInvalidResolution)
			}
			pickSHA, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--end-of-options", fields[1]+"^{commit}")
			if err != nil {
				return State{}, fmt.Errorf("%w: paused rebase completed commit is invalid", ErrInvalidResolution)
			}
			pickSHA = strings.TrimSpace(pickSHA)
			if err := gitexec.VerifyPublicationAncestry(ctx, path, pickSHA, origHead); err != nil {
				var commandErr *gitexec.CommandError
				if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
					return State{}, fmt.Errorf("%w: paused rebase completed commit is not on the assigned branch", ErrInvalidResolution)
				}
				return State{}, fmt.Errorf("verify paused rebase completed commit: %w", err)
			}
			if len(doneSources) == 0 || doneSources[len(doneSources)-1] != pickSHA {
				doneSources = append(doneSources, pickSHA)
			}
		}
		todoPath, err := gitexec.PathFor(ctx, path, "rebase-merge/git-rebase-todo")
		if err != nil {
			return State{}, err
		}
		rawTodo, err := ReadRegularGitFile(todoPath)
		if err != nil {
			return State{}, fmt.Errorf("%w: paused rebase todo is unavailable", ErrInvalidResolution)
		}
		var todoSources []string
		for line := range strings.SplitSeq(string(rawTodo), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 || (fields[0] != "pick" && fields[0] != "p") {
				return State{}, fmt.Errorf("%w: paused rebase contains operations outside source replay", ErrInvalidResolution)
			}
			pickSHA, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--end-of-options", fields[1]+"^{commit}")
			if err != nil {
				return State{}, fmt.Errorf("%w: paused rebase pending commit is invalid", ErrInvalidResolution)
			}
			todoSources = append(todoSources, strings.TrimSpace(pickSHA))
		}
		var rescheduledCommit string
		if stoppedCommit != "" && len(doneSources) > 0 && doneSources[len(doneSources)-1] == stoppedCommit {
			doneSources = doneSources[:len(doneSources)-1]
		} else if stoppedCommit == "" && len(doneSources) > 0 && len(todoSources) > 0 && doneSources[len(doneSources)-1] == todoSources[0] {
			doneSources = doneSources[:len(doneSources)-1]
			rescheduledPick = true
			rescheduledCommit = todoSources[0]
		}
		consumedRewritten := make(map[string]struct{}, len(rewritten))
		for _, src := range doneSources {
			if dst, ok := rewritten[src]; ok {
				consumedRewritten[src] = struct{}{}
				if err := verifyReplayedCommit(ctx, path, expectedHead, src, dst); err != nil {
					return State{}, err
				}
				expectedHead = dst
				maxReplayed++
				accounted = append(accounted, src)
				continue
			}
			fastForwarded := false
			srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
			if pErr != nil {
				var commandErr *gitexec.CommandError
				if !errors.As(pErr, &commandErr) || commandErr.ExitCode != 1 {
					return State{}, pErr
				}
			} else if strings.TrimSpace(srcParent) == expectedHead {
				isAnc, ancErr := gitexec.VerifyPublicationAncestryComplete(ctx, path, src, head)
				if ancErr != nil {
					return State{}, ancErr
				}
				if isAnc {
					expectedHead = src
					maxReplayed++
					fastForwarded = true
				}
			}
			if !fastForwarded {
				if err := verifyCommitRedundant(ctx, path, expectedHead, src); err != nil {
					return State{}, err
				}
			}
			accounted = append(accounted, src)
		}
		if len(consumedRewritten) != len(rewritten) {
			return State{}, fmt.Errorf("%w: paused rebase rewritten commit list does not match completed rebase steps", ErrInvalidResolution)
		}
		if stoppedCommit != "" {
			accounted = append(accounted, stoppedCommit)
		}
		accounted = append(accounted, todoSources...)
		pendingCommits = todoSources
		if err := verifyAssignedCommitsAccounted(ctx, path, onto, origHead, accounted); err != nil {
			return State{}, err
		}
		if rescheduledPick && stoppedCommit == "" {
			stoppedCommit = rescheduledCommit
		}
	} else {
		nextPath, err := gitexec.PathFor(ctx, path, "rebase-apply/next")
		if err != nil {
			return State{}, err
		}
		rawNext, err := ReadRegularGitFile(nextPath)
		if err != nil {
			return State{}, fmt.Errorf("%w: paused apply rebase state is unavailable", ErrInvalidResolution)
		}
		nextNum, err := strconv.Atoi(strings.TrimSpace(string(rawNext)))
		if err != nil || nextNum < 1 {
			return State{}, fmt.Errorf("%w: paused apply rebase patch index is invalid", ErrInvalidResolution)
		}
		lastPath, err := gitexec.PathFor(ctx, path, "rebase-apply/last")
		if err != nil {
			return State{}, err
		}
		rawLast, err := ReadRegularGitFile(lastPath)
		if err != nil {
			return State{}, fmt.Errorf("%w: paused apply rebase patch count is unavailable", ErrInvalidResolution)
		}
		lastNum, err := strconv.Atoi(strings.TrimSpace(string(rawLast)))
		if err != nil || lastNum < nextNum || lastNum > 10000 {
			return State{}, fmt.Errorf("%w: paused apply rebase patch count is invalid", ErrInvalidResolution)
		}
		maxReplayed = nextNum - 1
		var appliedSources []string
		var activeSource string
		for i := 1; i <= lastNum; i++ {
			patchPath, err := gitexec.PathFor(ctx, path, fmt.Sprintf("rebase-apply/%04d", i))
			if err != nil {
				return State{}, err
			}
			firstLine, err := readRegularGitFirstLine(patchPath)
			if err != nil {
				return State{}, fmt.Errorf("%w: paused apply rebase patch metadata is unavailable", ErrInvalidResolution)
			}
			fields := strings.Fields(firstLine)
			if len(fields) < 2 || fields[0] != "From" || !gitexec.ValidHead(fields[1]) {
				return State{}, fmt.Errorf("%w: paused apply rebase patch source is invalid", ErrInvalidResolution)
			}
			if err := gitexec.VerifyPublicationAncestry(ctx, path, fields[1], origHead); err != nil {
				var commandErr *gitexec.CommandError
				if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
					return State{}, fmt.Errorf("%w: paused apply rebase patch is not on the assigned branch", ErrInvalidResolution)
				}
				return State{}, fmt.Errorf("verify paused apply rebase source commit: %w", err)
			}
			accounted = append(accounted, fields[1])
			if i < nextNum {
				appliedSources = append(appliedSources, fields[1])
			} else {
				if err := verifyApplyPatchMatchesCommit(ctx, path, patchPath, fields[1], true); err != nil {
					return State{}, err
				}
				if i == nextNum {
					activeSource = fields[1]
				}
			}
		}
		if err := verifyAssignedCommitsAccounted(ctx, path, onto, origHead, accounted); err != nil {
			return State{}, err
		}
		if activeSource != "" {
			activePatchPath, err := gitexec.PathFor(ctx, path, "rebase-apply/patch")
			if err != nil {
				return State{}, err
			}
			if err := verifyActiveApplyPatchMatchesCommit(ctx, path, activePatchPath, activeSource); err != nil {
				return State{}, err
			}
		}
		abortSafetyPath, err := gitexec.PathFor(ctx, path, "rebase-apply/abort-safety")
		if err != nil {
			return State{}, err
		}
		rawSafety, err := ReadRegularGitFile(abortSafetyPath)
		if err == nil {
			safety := strings.TrimSpace(string(rawSafety))
			if !gitexec.ValidHead(safety) {
				return State{}, fmt.Errorf("%w: paused apply rebase safety head is invalid", ErrInvalidResolution)
			}
			expectedHead = safety
		} else if !errors.Is(err, os.ErrNotExist) {
			return State{}, err
		} else if maxReplayed > 0 {
			return State{}, fmt.Errorf("%w: paused apply rebase safety head is unavailable", ErrInvalidResolution)
		}
		if head != expectedHead {
			return State{}, fmt.Errorf("%w: paused rebase head does not match active rebase state", ErrInvalidResolution)
		}
		if err := gitexec.VerifyPublicationAncestry(ctx, path, onto, head); err != nil {
			var commandErr *gitexec.CommandError
			if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
				return State{}, fmt.Errorf("%w: paused rebase head does not descend from its target", ErrInvalidResolution)
			}
			return State{}, fmt.Errorf("verify paused rebase head ancestry: %w", err)
		}
		rawReplayed, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--reverse", onto+".."+head)
		if err != nil {
			return State{}, err
		}
		replayed := strings.Fields(rawReplayed)
		if len(replayed) > len(appliedSources) {
			return State{}, fmt.Errorf("%w: paused rebase head contains unexpected commits", ErrInvalidResolution)
		}
		prev := onto
		srcIdx := 0
		for _, dst := range replayed {
			matched := false
			for srcIdx < len(appliedSources) {
				src := appliedSources[srcIdx]
				srcIdx++
				replayCheckErr := verifyReplayedCommit(ctx, path, prev, src, dst)
				if replayCheckErr == nil {
					matched = true
					break
				}
				if !errors.Is(replayCheckErr, ErrInvalidResolution) {
					return State{}, replayCheckErr
				}
				if err := verifyCommitRedundant(ctx, path, prev, src); err != nil {
					return State{}, err
				}
			}
			if !matched {
				return State{}, fmt.Errorf("%w: paused apply rebase replayed commit does not match assigned source", ErrInvalidResolution)
			}
			prev = dst
		}
		for srcIdx < len(appliedSources) {
			if err := verifyCommitRedundant(ctx, path, prev, appliedSources[srcIdx]); err != nil {
				return State{}, err
			}
			srcIdx++
		}
		var canSkipEmptyApply bool
		if activeSource != "" {
			skipErr := verifyCommitRedundant(ctx, path, head, activeSource)
			if skipErr == nil {
				canSkipEmptyApply = true
			} else if !errors.Is(skipErr, ErrInvalidResolution) {
				return State{}, skipErr
			}
		}
		return State{
			Onto:           onto,
			TargetBase:     base,
			ResolvedTarget: target,
			ActiveCommit:   activeSource,
			CanSkipEmpty:   canSkipEmptyApply,
		}, nil
	}
	if head != expectedHead {
		return State{}, fmt.Errorf("%w: paused rebase head does not match active rebase state", ErrInvalidResolution)
	}
	if err := gitexec.VerifyPublicationAncestry(ctx, path, onto, head); err != nil {
		var commandErr *gitexec.CommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
			return State{}, fmt.Errorf("%w: paused rebase head does not descend from its target", ErrInvalidResolution)
		}
		return State{}, fmt.Errorf("verify paused rebase head ancestry: %w", err)
	}
	rawCount, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--count", onto+".."+head)
	if err != nil {
		return State{}, err
	}
	replayedCount, err := strconv.Atoi(strings.TrimSpace(rawCount))
	if err != nil || replayedCount > maxReplayed {
		return State{}, fmt.Errorf("%w: paused rebase head contains unexpected commits", ErrInvalidResolution)
	}
	var canDropEmptyActive bool
	if stoppedCommit != "" && !rescheduledPick {
		dropErr := verifyCommitRedundant(ctx, path, head, stoppedCommit)
		if dropErr == nil {
			canDropEmptyActive = true
		} else if !errors.Is(dropErr, ErrInvalidResolution) {
			return State{}, dropErr
		}
	}
	return State{
		Onto:            onto,
		TargetBase:      base,
		ResolvedTarget:  target,
		ActiveCommit:    stoppedCommit,
		CanSkipEmpty:    canDropEmptyActive,
		RescheduledPick: rescheduledPick,
		PendingCommits:  pendingCommits,
	}, nil
}

func openRegularGitFile(path string) (*os.File, os.FileInfo, error) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	linfo, err := root.Lstat(name)
	if err != nil {
		return nil, nil, errors.Join(err, root.Close())
	}
	if !linfo.Mode().IsRegular() {
		return nil, nil, errors.Join(fmt.Errorf("%w: git metadata file is not regular", ErrInvalidResolution), root.Close())
	}
	file, err := root.OpenFile(name, os.O_RDONLY|workpath.InspectFileOpenFlags, 0)
	if closeErr := root.Close(); err != nil || closeErr != nil {
		if file != nil {
			return nil, nil, errors.Join(err, closeErr, file.Close())
		}
		return nil, nil, errors.Join(err, closeErr)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() || !os.SameFile(linfo, info) {
		return nil, nil, errors.Join(fmt.Errorf("%w: git metadata file is not regular", ErrInvalidResolution), file.Close())
	}
	return file, info, nil
}

func finishRead(file *os.File, err *error) {
	if closeErr := file.Close(); closeErr != nil {
		*err = errors.Join(*err, closeErr)
	}
}

func ReadRegularGitFile(path string) (_ []byte, resultErr error) {
	file, _, err := openRegularGitFile(path)
	if err != nil {
		return nil, err
	}
	defer finishRead(file, &resultErr)
	return io.ReadAll(file)
}

func hashRegularGitFile(h io.Writer, path string, buf []byte) (resultErr error) {
	file, _, err := openRegularGitFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer finishRead(file, &resultErr)
	_, err = io.CopyBuffer(h, file, buf)
	return err
}

func readRegularGitFirstLine(path string) (_ string, resultErr error) {
	file, _, err := openRegularGitFile(path)
	if err != nil {
		return "", err
	}
	defer finishRead(file, &resultErr)
	line, err := bufio.NewReader(io.LimitReader(file, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func IndexChecksum(ctx context.Context, path string) (_ string, resultErr error) {
	indexPath, err := gitexec.PathFor(ctx, path, "index")
	if err != nil {
		return "", err
	}
	file, _, err := openRegularGitFile(indexPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer finishRead(file, &resultErr)
	h := sha256.New()
	if _, err := io.CopyBuffer(h, file, make([]byte, 32*1024)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type MetadataSnapshot struct {
	Metadata string
	index    string
}

func Snapshot(ctx context.Context, path string) (MetadataSnapshot, error) {
	head, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return MetadataSnapshot{}, err
	}
	gitDir, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return MetadataSnapshot{}, err
	}
	gitDir = strings.TrimSpace(gitDir)
	indexSum, err := IndexChecksum(ctx, path)
	if err != nil {
		return MetadataSnapshot{}, err
	}
	rawHead, err := ReadRegularGitFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return MetadataSnapshot{}, err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(head)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(rawHead)
	_, _ = h.Write([]byte{0})
	buf := make([]byte, 32*1024)
	for _, rel := range []string{
		"rebase-merge/onto",
		"rebase-merge/head-name",
		"rebase-merge/orig-head",
		"rebase-merge/rewritten-list",
		"rebase-merge/git-rebase-todo",
		"rebase-merge/stopped-sha",
		"rebase-merge/update-refs",
		"rebase-merge/autostash",
		"rebase-merge/strategy",
		"rebase-merge/strategy_opts",
		"rebase-apply/onto",
		"rebase-apply/head-name",
		"rebase-apply/orig-head",
		"rebase-apply/next",
		"rebase-apply/last",
		"rebase-apply/abort-safety",
		"rebase-apply/patch",
		"rebase-apply/update-refs",
		"rebase-apply/autostash",
		"rebase-apply/strategy",
		"rebase-apply/strategy_opts",
	} {
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		if err := hashRegularGitFile(h, filepath.Join(gitDir, filepath.FromSlash(rel)), buf); err != nil {
			return MetadataSnapshot{}, err
		}
		_, _ = h.Write([]byte{0})
	}
	if rawLast, err := ReadRegularGitFile(filepath.Join(gitDir, "rebase-apply", "last")); err == nil {
		if lastNum, convErr := strconv.Atoi(strings.TrimSpace(string(rawLast))); convErr == nil && lastNum >= 1 && lastNum <= 10000 {
			for i := 1; i <= lastNum; i++ {
				rel := fmt.Sprintf("rebase-apply/%04d", i)
				_, _ = h.Write([]byte(rel))
				_, _ = h.Write([]byte{0})
				if rErr := hashRegularGitFile(h, filepath.Join(gitDir, filepath.FromSlash(rel)), buf); rErr != nil {
					return MetadataSnapshot{}, rErr
				}
				_, _ = h.Write([]byte{0})
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return MetadataSnapshot{}, err
	}
	for _, headNameRel := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
		rawHeadName, rErr := ReadRegularGitFile(filepath.Join(gitDir, filepath.FromSlash(headNameRel)))
		if rErr != nil {
			if !errors.Is(rErr, os.ErrNotExist) {
				return MetadataSnapshot{}, rErr
			}
			continue
		}
		headName := strings.TrimSpace(string(rawHeadName))
		if headName == "" {
			continue
		}
		tip, tipErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", headName+"^{commit}")
		if tipErr != nil {
			var commandErr *gitexec.CommandError
			if !errors.As(tipErr, &commandErr) || commandErr.ExitCode != 1 {
				return MetadataSnapshot{}, tipErr
			}
			tip = ""
		}
		_, _ = h.Write([]byte("head-tip:" + headName))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strings.TrimSpace(tip)))
		_, _ = h.Write([]byte{0})
	}
	return MetadataSnapshot{
		Metadata: hex.EncodeToString(h.Sum(nil)),
		index:    indexSum,
	}, nil
}

func verifyActiveApplyPatchMatchesCommit(ctx context.Context, path, patchPath, commitSHA string) (resultErr error) {
	file, _, err := openRegularGitFile(patchPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer finishRead(file, &resultErr)
	var prefix [5]byte
	n, rErr := file.Read(prefix[:])
	if rErr != nil && !errors.Is(rErr, io.EOF) {
		return rErr
	}
	if n == 0 {
		return nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return verifyApplyPatchFileMatchesCommit(ctx, path, file, commitSHA, bytes.HasPrefix(prefix[:n], []byte("From ")))
}

func verifyApplyPatchMatchesCommit(ctx context.Context, path, patchPath, commitSHA string, isMail bool) (resultErr error) {
	file, _, err := openRegularGitFile(patchPath)
	if err != nil {
		return err
	}
	defer finishRead(file, &resultErr)
	return verifyApplyPatchFileMatchesCommit(ctx, path, file, commitSHA, isMail)
}

func verifyApplyPatchFileMatchesCommit(ctx context.Context, path string, patchFile *os.File, commitSHA string, isMail bool) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", commitSHA+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused apply rebase patch parent is unavailable", ErrInvalidResolution)
	}
	wantTree, tErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", commitSHA+"^{tree}")
	if tErr != nil {
		return tErr
	}
	gotTree, applyErr := gitexec.IsolatedApplyTree(ctx, path, strings.TrimSpace(srcParent), patchFile, isMail)
	if applyErr != nil {
		var commandErr *gitexec.CommandError
		if errors.As(applyErr, &commandErr) {
			return fmt.Errorf("%w: paused apply rebase patch does not apply cleanly to source parent", ErrInvalidResolution)
		}
		return applyErr
	}
	if gotTree != strings.TrimSpace(wantTree) {
		return fmt.Errorf("%w: paused apply rebase patch body does not match assigned commit", ErrInvalidResolution)
	}
	return nil
}

func HasUntrackedResetObstruction(root string, deletedPaths, addedPaths []string) bool {
	type trackedEntry struct {
		info       os.FileInfo
		parentInfo os.FileInfo
		base       string
	}
	trackedAdded := make([]trackedEntry, 0, len(addedPaths))
	for _, rel := range addedPaths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		parentInfo, pErr := os.Stat(filepath.Dir(full))
		if pErr != nil {
			continue
		}
		trackedAdded = append(trackedAdded, trackedEntry{
			info:       info,
			parentInfo: parentInfo,
			base:       filepath.Base(full),
		})
	}
	isTrackedAdded := func(full string, fi os.FileInfo) bool {
		parentInfo, err := os.Stat(filepath.Dir(full))
		if err != nil {
			return false
		}
		base := filepath.Base(full)
		for _, tracked := range trackedAdded {
			if !os.SameFile(fi, tracked.info) || !os.SameFile(parentInfo, tracked.parentInfo) {
				continue
			}
			if base == tracked.base {
				return true
			}
			if strings.EqualFold(base, tracked.base) {
				entries, rErr := os.ReadDir(filepath.Dir(full))
				if rErr != nil {
					return false
				}
				matches := 0
				for _, entry := range entries {
					if strings.EqualFold(entry.Name(), base) {
						matches++
					}
				}
				if matches == 1 {
					return true
				}
			}
		}
		return false
	}
	errObstructed := errors.New("obstructed")
	for _, del := range deletedPaths {
		parts := strings.Split(del, "/")
		cur := root
		for i, part := range parts {
			if part == "" {
				continue
			}
			cur = filepath.Join(cur, part)
			fi, err := os.Lstat(cur)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return true
			}
			if !fi.IsDir() {
				if isTrackedAdded(cur, fi) {
					break
				}
				return true
			}
			if i == len(parts)-1 {
				walkErr := filepath.WalkDir(cur, func(p string, d os.DirEntry, wErr error) error {
					if wErr != nil {
						return errObstructed
					}
					if d.IsDir() {
						return nil
					}
					entryInfo, statErr := os.Lstat(p)
					if statErr != nil || !isTrackedAdded(p, entryInfo) {
						return errObstructed
					}
					return nil
				})
				if walkErr != nil {
					return true
				}
			}
		}
	}
	return false
}

func VerifyStagedExactReplayTree(ctx context.Context, path, prev, src string) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase rescheduled commit parent is unavailable", ErrInvalidResolution)
	}
	return gitexec.IsolatedMergeTreeIndex(ctx, path, strings.TrimSpace(srcParent), prev, src, func(_ func(...string) (string, error), diffIndex func(tree string) (string, error), merged string, mErr error) error {
		if mErr != nil {
			return fmt.Errorf("%w: paused rebase rescheduled commit has unexpected merge state", ErrInvalidResolution)
		}
		mergedTree, _, _ := strings.Cut(merged, "\x00")
		diffRaw, dErr := diffIndex(strings.TrimSpace(mergedTree))
		if dErr != nil {
			return dErr
		}
		if strings.TrimSpace(diffRaw) != "" {
			return fmt.Errorf("%w: paused rebase rescheduled pick has extra staged changes", ErrInvalidResolution)
		}
		return nil
	})
}

func VerifyStagedConflictResolution(ctx context.Context, path, prev, src string) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase active commit parent is unavailable", ErrInvalidResolution)
	}
	return gitexec.IsolatedMergeTreeIndex(ctx, path, strings.TrimSpace(srcParent), prev, src, func(_ func(...string) (string, error), diffIndex func(tree string) (string, error), merged string, mErr error) error {
		if mErr == nil {
			mergedTree, _, _ := strings.Cut(merged, "\x00")
			diffRaw, dErr := diffIndex(strings.TrimSpace(mergedTree))
			if dErr != nil {
				return dErr
			}
			if strings.TrimSpace(diffRaw) != "" {
				return fmt.Errorf("%w: paused rebase staged resolution does not match clean merge tree", ErrInvalidResolution)
			}
			return nil
		}
		var commandErr *gitexec.CommandError
		if !errors.As(mErr, &commandErr) || commandErr.ExitCode != 1 {
			return fmt.Errorf("verify paused rebase staged resolution: %w", mErr)
		}
		return nil
	})
}

func verifyAssignedCommitsAccounted(ctx context.Context, path, onto, origHead string, accounted []string) error {
	rawAllowed, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--reverse", "--topo-order", "--no-merges", onto+".."+origHead)
	if err != nil {
		return err
	}
	allowedOrder := make(map[string]int)
	for idx, sha := range strings.Fields(rawAllowed) {
		allowedOrder[sha] = idx
	}
	seen := make(map[string]struct{}, len(accounted))
	lastIdx := -1
	for _, sha := range accounted {
		idx, ok := allowedOrder[sha]
		if !ok {
			return fmt.Errorf("%w: paused rebase includes a commit outside the assigned branch range", ErrInvalidResolution)
		}
		if _, dup := seen[sha]; dup {
			return fmt.Errorf("%w: paused rebase duplicates an assigned commit", ErrInvalidResolution)
		}
		if idx <= lastIdx {
			return fmt.Errorf("%w: paused rebase reorders assigned commits", ErrInvalidResolution)
		}
		lastIdx = idx
		seen[sha] = struct{}{}
	}
	rawExpected, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--cherry-pick", "--right-only", "--no-merges", onto+"..."+origHead)
	if err != nil {
		return err
	}
	for _, sha := range strings.Fields(rawExpected) {
		if _, ok := seen[sha]; ok {
			continue
		}
		empty, emptyErr := isEmptyCommit(ctx, path, sha)
		if emptyErr != nil {
			return emptyErr
		}
		if empty {
			continue
		}
		return fmt.Errorf("%w: paused rebase is missing an assigned commit", ErrInvalidResolution)
	}
	return nil
}

func isEmptyCommit(ctx context.Context, path, sha string) (bool, error) {
	parent, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", sha+"^")
	if err != nil {
		var commandErr *gitexec.CommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
			return false, nil
		}
		return false, err
	}
	commitTree, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", sha+"^{tree}")
	if err != nil {
		return false, err
	}
	parentTree, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", strings.TrimSpace(parent)+"^{tree}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(commitTree) == strings.TrimSpace(parentTree), nil
}

func verifyCommitRedundant(ctx context.Context, path, prev, src string) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase dropped commit cannot be verified as redundant", ErrInvalidResolution)
	}
	return gitexec.IsolatedMergeTree(ctx, path, strings.TrimSpace(srcParent), prev, src, func(read func(...string) (string, error), merged string, mErr error) error {
		prevTree, tErr := read("rev-parse", "--verify", prev+"^{tree}")
		if tErr != nil {
			return tErr
		}
		if mErr == nil {
			mergedTree, _, _ := strings.Cut(merged, "\x00")
			if strings.TrimSpace(mergedTree) != strings.TrimSpace(prevTree) {
				return fmt.Errorf("%w: paused rebase dropped commit is not redundant against target", ErrInvalidResolution)
			}
			return nil
		}
		var commandErr *gitexec.CommandError
		if !errors.As(mErr, &commandErr) || commandErr.ExitCode != 1 {
			return fmt.Errorf("verify paused rebase dropped commit: %w", mErr)
		}
		header, _, _ := strings.Cut(commandErr.Output, "\x00\x00")
		parts := strings.Split(header, "\x00")
		if len(parts) == 0 || !gitexec.ValidHead(strings.TrimSpace(parts[0])) {
			return fmt.Errorf("%w: paused rebase dropped conflict merge tree is unavailable", ErrInvalidResolution)
		}
		mergedTree := strings.TrimSpace(parts[0])
		conflictedPaths := make(map[string]struct{}, len(parts)-1)
		for _, name := range parts[1:] {
			if name != "" {
				conflictedPaths[name] = struct{}{}
			}
		}
		diffPathsRaw, dErr := read("diff-tree", "--no-commit-id", "--name-only", "-r", "-z", strings.TrimSpace(prevTree), mergedTree)
		if dErr != nil {
			return dErr
		}
		for _, name := range strings.Split(strings.TrimSuffix(diffPathsRaw, "\x00"), "\x00") {
			if name == "" {
				continue
			}
			if _, ok := conflictedPaths[name]; !ok {
				return fmt.Errorf("%w: paused rebase dropped commit has non-conflicted changes", ErrInvalidResolution)
			}
		}
		return nil
	})
}

func verifyReplayedCommit(ctx context.Context, path, prev, src, dst string) error {
	if dst == prev {
		return verifyCommitRedundant(ctx, path, prev, src)
	}
	parent, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", dst+"^")
	if err != nil || strings.TrimSpace(parent) != prev {
		return fmt.Errorf("%w: paused rebase replayed commit parent does not match active target", ErrInvalidResolution)
	}
	srcMeta, err := gitexec.ReadPublicationSource(ctx, path, "show", "-s", "--format=%an%x00%ae%x00%at%x00%s", src)
	if err != nil {
		return err
	}
	dstMeta, err := gitexec.ReadPublicationSource(ctx, path, "show", "-s", "--format=%an%x00%ae%x00%at%x00%s", dst)
	if err != nil {
		return err
	}
	if srcMeta != dstMeta {
		return fmt.Errorf("%w: paused rebase replayed commit metadata does not match assigned source", ErrInvalidResolution)
	}
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase source commit parent is unavailable", ErrInvalidResolution)
	}
	return gitexec.IsolatedMergeTree(ctx, path, strings.TrimSpace(srcParent), prev, src, func(read func(...string) (string, error), merged string, mErr error) error {
		if mErr == nil {
			mergedTree, _, _ := strings.Cut(merged, "\x00")
			dstTree, tErr := read("rev-parse", "--verify", dst+"^{tree}")
			if tErr != nil {
				return tErr
			}
			if strings.TrimSpace(mergedTree) != strings.TrimSpace(dstTree) {
				return fmt.Errorf("%w: paused rebase replayed commit content does not match assigned source", ErrInvalidResolution)
			}
			return nil
		}
		var commandErr *gitexec.CommandError
		if !errors.As(mErr, &commandErr) || commandErr.ExitCode != 1 {
			return fmt.Errorf("verify paused rebase replayed tree: %w", mErr)
		}
		prevTree, tErr := read("rev-parse", "--verify", prev+"^{tree}")
		if tErr != nil {
			return tErr
		}
		dstTree, tErr := read("rev-parse", "--verify", dst+"^{tree}")
		if tErr != nil {
			return tErr
		}
		if strings.TrimSpace(prevTree) == strings.TrimSpace(dstTree) {
			return fmt.Errorf("%w: paused rebase replayed commit drops all changes", ErrInvalidResolution)
		}
		return nil
	})
}

func InProgress(ctx context.Context, workspacePath string) (bool, error) {
	for _, gitPath := range []string{"rebase-merge", "rebase-apply"} {
		path, err := gitexec.PathFor(ctx, workspacePath, gitPath)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}
