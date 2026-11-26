package checkpoint

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

const (
	phaseCommitLimit   = 2048
	phasePathLimit     = 8192
	phaseCommitBytes   = 1 << 20
	phaseMetadataBytes = 4 << 20
	phaseBlobLimit     = 32 << 20
	phaseBlobOverhead  = 1 << 10
)

func PhaseHistory(git gitexec.BoundedRead, head string, published ...string) error {
	args := []string{"rev-list", "--max-count=" + strconv.Itoa(phaseCommitLimit+1), head}
	if len(published) != 0 {
		args = append(append(args, "--not"), published...)
	}
	rawCommits, err := readPhaseOutput(git, (phaseCommitLimit+1)*65, "commit list", args...)
	if err != nil {
		return err
	}
	commits := strings.Fields(rawCommits)
	if len(commits) > phaseCommitLimit {
		return fmt.Errorf("%w: phase history exceeds the commit limit", ErrUnsafe)
	}
	remaining := tracker.MaxChangeDiffBytes - len(rawCommits)
	pathsRemaining := phasePathLimit
	for _, commit := range commits {
		if !gitexec.ValidHead(commit) {
			return fmt.Errorf("%w: invalid phase commit identity", ErrUnsafe)
		}
		raw, err := readPhaseOutput(git, min(remaining, phaseCommitBytes), "commit object", "cat-file", "commit", commit)
		if err != nil {
			return err
		}
		remaining -= len(raw)
		if !strings.Contains(raw, "\n\n") {
			return fmt.Errorf("%w: invalid phase commit object", ErrUnsafe)
		}
		if err := screenCredentials(raw); err != nil {
			return err
		}
		changed, err := readPhaseOutput(git, min(remaining, phaseMetadataBytes), "changed paths",
			"diff-tree", "--root", "--no-commit-id", "--raw", "--no-abbrev", "--no-renames", "-r", "-m", "-z", commit)
		if err != nil {
			return err
		}
		remaining -= len(changed)
		if err := screenPhasePaths(git, changed, &pathsRemaining, &remaining); err != nil {
			return err
		}
	}
	return nil
}

func screenPhasePaths(git gitexec.BoundedRead, changed string, pathsRemaining, remaining *int) error {
	for changed != "" {
		if *pathsRemaining == 0 {
			return fmt.Errorf("%w: phase history exceeds the changed path limit", ErrUnsafe)
		}
		*pathsRemaining--
		header, rest, hasHeader := strings.Cut(changed, "\x00")
		path, next, hasPath := strings.Cut(rest, "\x00")
		changed = next
		fields := strings.Fields(header)
		if !hasHeader || !hasPath || len(fields) != 5 || !strings.HasPrefix(fields[0], ":") {
			return fmt.Errorf("%w: invalid phase changed path record", ErrUnsafe)
		}
		if !PathAllowed(path) || strings.HasPrefix(strings.ToLower(path), ".detent/") {
			return fmt.Errorf("%w: commit contains an unapproved path", ErrUnsafe)
		}
		if err := screenCredentials(path); err != nil {
			return err
		}
		switch fields[1] {
		case "000000":
			continue
		case "100644", "100755":
		default:
			return fmt.Errorf("%w: selected history must contain regular files", ErrUnsafe)
		}
		if err := screenPhaseBlob(git, fields[3], remaining); err != nil {
			return err
		}
	}
	return nil
}

func screenPhaseBlob(git gitexec.BoundedRead, object string, remaining *int) error {
	if !gitexec.ValidHead(object) {
		return fmt.Errorf("%w: invalid phase source blob identity", ErrUnsafe)
	}
	output, err := readPhaseOutput(git, 32, "blob size", "cat-file", "-s", object)
	if err != nil {
		return err
	}
	size, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || size < 0 {
		return fmt.Errorf("%w: invalid phase source blob size", ErrUnsafe)
	}
	if size > phaseBlobLimit {
		return fmt.Errorf("%w: phase source blob exceeds the scan limit", ErrUnsafe)
	}
	cost := size + phaseBlobOverhead
	if cost > *remaining {
		return fmt.Errorf("%w: phase source exceeds the aggregate scan limit", ErrUnsafe)
	}
	*remaining -= cost
	blob, err := readPhaseOutput(git, size, "blob", "cat-file", "blob", object)
	if err != nil {
		return err
	}
	return screenCredentials(blob)
}

func readPhaseOutput(git gitexec.BoundedRead, limit int, step string, args ...string) (string, error) {
	output, truncated, err := git(limit, args...)
	if err != nil {
		return "", fmt.Errorf("read phase %s: %w", step, err)
	}
	if truncated {
		return "", fmt.Errorf("%w: phase %s exceeds the output limit", ErrUnsafe, step)
	}
	return output, nil
}
