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
	phaseCommitLimit    = 2048
	phaseParentLimit    = 4096
	phasePublishedLimit = 8192
	phasePathLimit      = 8192
	phaseCommitBytes    = 1 << 20
	phaseMetadataBytes  = 4 << 20
	phaseBlobLimit      = 32 << 20
	phaseBlobOverhead   = 1 << 10
)

func PhaseHistory(git gitexec.BoundedRead, head string, published ...string) error {
	scan := phaseScan{git: git, remaining: tracker.MaxChangeDiffBytes, paths: phasePathLimit}
	return scan.history(head, published)
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

func (s *phaseScan) history(head string, published []string) error {
	if !gitexec.ValidHead(head) {
		return fmt.Errorf("%w: invalid phase commit identity", ErrUnsafe)
	}
	seen := make(map[string]bool)
	for _, tip := range published {
		if !gitexec.ValidHead(tip) {
			return fmt.Errorf("%w: invalid published phase commit identity", ErrUnsafe)
		}
		seen[tip] = true
	}
	if seen[head] {
		return nil
	}
	if len(published) != 0 {
		args := append([]string{"rev-list", "--max-count=" + strconv.Itoa(phasePublishedLimit)}, published...)
		raw, err := s.read(phasePublishedLimit*65, "published commit list", args...)
		if err != nil {
			return err
		}
		for oid := range strings.FieldsSeq(raw) {
			if !gitexec.ValidHead(oid) {
				return fmt.Errorf("%w: invalid published phase commit identity", ErrUnsafe)
			}
			seen[oid] = true
		}
	}
	commits := make([]phaseCommit, 0)
	trees := make(map[string]string)
	pending := []string{head}
	edges := 0
	for len(pending) != 0 {
		oid := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[oid] {
			continue
		}
		if len(commits) == phaseCommitLimit {
			return fmt.Errorf("%w: phase history exceeds the commit limit", ErrUnsafe)
		}
		commit, err := s.readCommit(oid, true)
		if err != nil {
			return err
		}
		edges += len(commit.parents)
		if edges > phaseParentLimit {
			return fmt.Errorf("%w: phase history exceeds the parent edge limit", ErrUnsafe)
		}
		seen[oid] = true
		trees[oid] = commit.tree
		commits = append(commits, commit)
		pending = append(pending, commit.parents...)
	}
	for _, commit := range commits {
		if len(commit.parents) == 0 {
			if err := s.walkTrees("", commit.tree); err != nil {
				return err
			}
		}
		for _, parent := range commit.parents {
			parentTree, ok := trees[parent]
			if !ok {
				metadata, err := s.readCommit(parent, false)
				if err != nil {
					return err
				}
				parentTree = metadata.tree
				trees[parent] = parentTree
			}
			if err := s.walkTrees(parentTree, commit.tree); err != nil {
				return err
			}
		}
	}
	return s.screenBlobs()
}

type phaseCommit struct {
	tree    string
	parents []string
}

func (s *phaseScan) readCommit(oid string, unpublished bool) (phaseCommit, error) {
	if err := s.checkObject(oid, "commit", phaseCommitBytes); err != nil {
		return phaseCommit{}, err
	}
	raw, err := s.read(phaseCommitBytes, "commit object", "cat-file", "commit", oid)
	if err != nil {
		return phaseCommit{}, err
	}
	headers, _, ok := strings.Cut(raw, "\n\n")
	treeLine, parents, _ := strings.Cut(headers, "\n")
	tree, hasTree := strings.CutPrefix(treeLine, "tree ")
	if !ok || !hasTree || !gitexec.ValidHead(tree) {
		return phaseCommit{}, fmt.Errorf("%w: invalid phase commit object", ErrUnsafe)
	}
	commit := phaseCommit{tree: tree}
	if !unpublished {
		return commit, nil
	}
	if err := ScreenCredentials(raw); err != nil {
		return phaseCommit{}, err
	}
	for strings.HasPrefix(parents, "parent ") {
		line, rest, _ := strings.Cut(parents, "\n")
		parents = rest
		parent := strings.TrimPrefix(line, "parent ")
		if !gitexec.ValidHead(parent) {
			return phaseCommit{}, fmt.Errorf("%w: invalid phase parent identity", ErrUnsafe)
		}
		commit.parents = append(commit.parents, parent)
	}
	return commit, nil
}

func (s *phaseScan) checkObject(oid, kind string, limit int) error {
	if !gitexec.ValidHead(oid) {
		return fmt.Errorf("%w: invalid phase object identity", ErrUnsafe)
	}
	sizeText, err := s.read(32, "object size", "cat-file", "-s", oid)
	if err != nil {
		return err
	}
	size, err := strconv.Atoi(strings.TrimSpace(sizeText))
	if err != nil || size < 0 {
		return fmt.Errorf("%w: invalid phase %s object size", ErrUnsafe, kind)
	}
	if size > min(limit, s.remaining) {
		return fmt.Errorf("%w: phase %s object exceeds the size limit", ErrUnsafe, kind)
	}
	typeText, err := s.read(16, "object type", "cat-file", "-t", oid)
	if err != nil {
		return err
	}
	if strings.TrimSpace(typeText) != kind {
		return fmt.Errorf("%w: invalid phase %s object type", ErrUnsafe, kind)
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
		return map[string]phaseTreeEntry{}, nil
	}
	if err := s.checkObject(tree, "tree", phaseMetadataBytes); err != nil {
		return nil, err
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
