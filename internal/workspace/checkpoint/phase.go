package checkpoint

import (
	"fmt"
	"maps"
	"slices"
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
	scan := phaseScan{git: git, remaining: tracker.MaxChangeDiffBytes - len(rawCommits), paths: phasePathLimit}
	for _, commit := range commits {
		if !gitexec.ValidHead(commit) {
			return fmt.Errorf("%w: invalid phase commit identity", ErrUnsafe)
		}
		raw, err := scan.read(phaseCommitBytes, "commit object", "cat-file", "commit", commit)
		if err != nil {
			return err
		}
		if err := scan.commit(raw); err != nil {
			return err
		}
	}
	return scan.screenBlobs()
}

type phaseScan struct {
	git       gitexec.BoundedRead
	remaining int
	paths     int
	blobs     []string
}

func (s *phaseScan) read(limit int, step string, args ...string) (string, error) {
	output, err := readPhaseOutput(s.git, min(s.remaining, limit), step, args...)
	s.remaining -= len(output)
	return output, err
}

func (s *phaseScan) commit(raw string) error {
	headers, _, ok := strings.Cut(raw, "\n\n")
	treeLine, parents, _ := strings.Cut(headers, "\n")
	tree, hasTree := strings.CutPrefix(treeLine, "tree ")
	if !ok || !hasTree || !gitexec.ValidHead(tree) {
		return fmt.Errorf("%w: invalid phase commit object", ErrUnsafe)
	}
	if err := ScreenCredentials(raw); err != nil {
		return err
	}
	if !strings.HasPrefix(parents, "parent ") {
		return s.walkTrees("", tree)
	}
	for strings.HasPrefix(parents, "parent ") {
		line, rest, _ := strings.Cut(parents, "\n")
		parents = rest
		parent := strings.TrimPrefix(line, "parent ")
		if !gitexec.ValidHead(parent) {
			return fmt.Errorf("%w: invalid phase parent identity", ErrUnsafe)
		}
		parentTree, err := s.read(65, "parent tree", "rev-parse", "--verify", parent+"^{tree}")
		if err != nil {
			return err
		}
		parentTree = strings.TrimSuffix(parentTree, "\n")
		if !gitexec.ValidHead(parentTree) {
			return fmt.Errorf("%w: invalid phase parent tree identity", ErrUnsafe)
		}
		if err := s.walkTrees(parentTree, tree); err != nil {
			return err
		}
	}
	return nil
}

type phaseTreePair struct {
	before string
	after  string
	prefix string
}

type phaseTreeEntry struct {
	mode   string
	object string
}

func (e phaseTreeEntry) tree() string {
	if e.mode == "040000" {
		return e.object
	}
	return ""
}

func (s *phaseScan) walkTrees(before, after string) error {
	pending := []phaseTreePair{{before: before, after: after}}
	for len(pending) > 0 {
		pair := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if pair.before == pair.after {
			continue
		}
		oldEntries, err := s.readTree(pair.before)
		if err != nil {
			return err
		}
		newEntries, err := s.readTree(pair.after)
		if err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(newEntries)) {
			if err := s.changePath(&pending, pair.prefix, name, oldEntries[name], newEntries[name]); err != nil {
				return err
			}
			delete(oldEntries, name)
		}
		for _, name := range slices.Sorted(maps.Keys(oldEntries)) {
			if err := s.changePath(&pending, pair.prefix, name, oldEntries[name], phaseTreeEntry{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *phaseScan) readTree(tree string) (map[string]phaseTreeEntry, error) {
	if tree == "" {
		return nil, nil
	}
	output, err := s.read(phaseMetadataBytes, "tree entries", "ls-tree", "-z", tree)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]phaseTreeEntry)
	for output != "" {
		record, rest, terminated := strings.Cut(output, "\x00")
		output = rest
		header, name, separated := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !terminated || !separated || len(fields) != 3 || name == "" || strings.Contains(name, "/") || !gitexec.ValidHead(fields[2]) {
			return nil, fmt.Errorf("%w: invalid phase tree entry", ErrUnsafe)
		}
		if _, duplicate := entries[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate phase tree entry", ErrUnsafe)
		}
		entries[name] = phaseTreeEntry{mode: fields[0], object: fields[2]}
	}
	return entries, nil
}

func (s *phaseScan) changePath(pending *[]phaseTreePair, prefix, name string, before, after phaseTreeEntry) error {
	if before == after {
		return nil
	}
	if s.paths == 0 {
		return fmt.Errorf("%w: phase history exceeds the changed path limit", ErrUnsafe)
	}
	s.paths--
	cost := len(prefix) + len(name) + 1
	if cost > s.remaining {
		return fmt.Errorf("%w: phase source exceeds the aggregate scan limit", ErrUnsafe)
	}
	s.remaining -= cost
	path := prefix + name
	if !PathAllowed(path) || strings.HasPrefix(strings.ToLower(path), ".detent/") {
		return fmt.Errorf("%w: commit contains an unapproved path", ErrUnsafe)
	}
	if err := ScreenCredentials(path); err != nil {
		return err
	}
	switch after.mode {
	case "", "040000":
	case "100644", "100755":
		s.blobs = append(s.blobs, after.object)
	default:
		return fmt.Errorf("%w: selected history must contain regular files", ErrUnsafe)
	}
	if oldTree, newTree := before.tree(), after.tree(); oldTree != newTree {
		*pending = append(*pending, phaseTreePair{before: oldTree, after: newTree, prefix: path + "/"})
	}
	return nil
}

func (s *phaseScan) screenBlobs() error {
	for _, blob := range s.blobs {
		if err := screenPhaseBlob(s.git, blob, &s.remaining); err != nil {
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
	return ScreenCredentials(blob)
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
