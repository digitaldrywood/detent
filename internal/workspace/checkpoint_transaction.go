package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type checkpointIndexEntry struct {
	Path  string
	Mode  string
	Blob  string
	Stage int
}

type checkpointTransaction struct {
	DropRedundant bool
	Schema        int
	Branch        string
	Head          string
	Original      string
	Onto          string
	Stopped       string
	Worktree      string
	Index         []checkpointIndexEntry
	Done          []string
	Todo          []string
	Rewritten     [][2]string
}

func (l *LocalGit) checkpointCommit(ctx context.Context, path, tree string, parents []string) (string, error) {
	args := []string{"commit-tree", tree}
	seen := make(map[string]bool)
	for _, parent := range parents {
		if !seen[parent] {
			args = append(args, "-p", parent)
			seen[parent] = true
		}
	}
	args = append(args, "-m", "detent: checkpoint")
	output, err := l.runCommit(ctx, path, nil, args...)
	return strings.TrimSpace(output), err
}

func checkpointReplay(ctx context.Context, git checkpointGitRunner, data string) ([]string, error) {
	var commits []string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "pick" && fields[0] != "p" {
			return nil, fmt.Errorf("%w: unsupported rebase operation", ErrCheckpointUnsafe)
		}
		output, err := git(ctx, "rev-parse", "--verify", fields[1]+"^{commit}")
		if err != nil || !validLandingHead(strings.TrimSpace(output)) {
			return nil, errors.Join(ErrCheckpointUnsafe, err)
		}
		commits = append(commits, strings.TrimSpace(output))
	}
	return commits, nil
}

func captureCheckpointTransaction(ctx context.Context, path, branch, scratch string) (*checkpointTransaction, error) {
	git := checkpointRefGit(path)
	dir, err := gitPathFor(ctx, path, "rebase-merge")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: only merge-backend source replay is portable", ErrCheckpointUnsafe)
	}
	allowed := []string{"head-name", "orig-head", "onto", "stopped-sha", "done", "git-rebase-todo", "git-rebase-todo.backup", "rewritten-list", "msgnum", "end", "message", "author-script", "patch", "interactive", "no-reschedule-failed-exec", "gpg_sign_opt", "drop_redundant_commits"}
	for _, entry := range entries {
		if !slices.Contains(allowed, entry.Name()) || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("%w: unsupported rebase metadata %q", ErrCheckpointUnsafe, entry.Name())
		}
	}
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(data)), err
	}
	t := &checkpointTransaction{Schema: 1, Branch: branch}
	if _, err := read("drop_redundant_commits"); err == nil {
		t.DropRedundant = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	name, err := read("head-name")
	if err != nil || name != "refs/heads/"+branch {
		return nil, errors.Join(ErrCheckpointUnsafe, err)
	}
	for name, field := range map[string]*string{"orig-head": &t.Original, "onto": &t.Onto, "stopped-sha": &t.Stopped} {
		*field, err = read(name)
		if err != nil || !validLandingHead(*field) {
			return nil, errors.Join(ErrCheckpointUnsafe, err)
		}
	}
	for name, field := range map[string]*[]string{"done": &t.Done, "git-rebase-todo": &t.Todo} {
		data, err := read(name)
		if err != nil {
			return nil, err
		}
		*field, err = checkpointReplay(ctx, git, data)
		if err != nil {
			return nil, err
		}
	}
	if len(t.Done) == 0 || t.Done[len(t.Done)-1] != t.Stopped {
		return nil, fmt.Errorf("%w: stopped source differs from replay", ErrCheckpointUnsafe)
	}
	for name, want := range map[string]int{"msgnum": len(t.Done), "end": len(t.Done) + len(t.Todo)} {
		value, err := read(name)
		if err != nil || value != strconv.Itoa(want) {
			return nil, errors.Join(ErrCheckpointUnsafe, err)
		}
	}
	data, err := read("rewritten-list")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validLandingHead(fields[0]) || !validLandingHead(fields[1]) {
			return nil, ErrCheckpointUnsafe
		}
		t.Rewritten = append(t.Rewritten, [2]string{fields[0], fields[1]})
	}
	expectedAuthor, err := checkpointAuthorScript(ctx, git, t.Stopped)
	if err != nil {
		return nil, err
	}
	author, err := read("author-script")
	if err != nil || author != strings.TrimSpace(expectedAuthor) {
		return nil, errors.Join(fmt.Errorf("%w: altered author metadata", ErrCheckpointUnsafe), err)
	}
	expectedMessage, err := git(ctx, "show", "-s", "--format=%B", t.Stopped)
	if err != nil {
		return nil, err
	}
	message, err := read("message")
	if body, conflicts, ok := strings.Cut(message, "\n\n# Conflicts:\n"); ok {
		for _, line := range strings.Split(conflicts, "\n") {
			if !strings.HasPrefix(line, "#\t") || !checkpointPathAllowed(strings.TrimPrefix(line, "#\t")) {
				return nil, ErrCheckpointUnsafe
			}
		}
		message = body
	}
	if err != nil || message != strings.TrimSpace(expectedMessage) {
		return nil, errors.Join(fmt.Errorf("%w: altered message metadata", ErrCheckpointUnsafe), err)
	}
	index := checkpointRefGit(path, "GIT_INDEX_FILE="+filepath.Join(scratch, "worktree-index"))
	t.Head, t.Worktree, _, err = checkpointSnapshot(ctx, git, index)
	if err != nil {
		return nil, err
	}
	output, err := git(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	for _, entry := range strings.Split(strings.TrimSuffix(output, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, ErrCheckpointUnsafe
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		t.Index = append(t.Index, checkpointIndexEntry{Path: path, Mode: fields[0], Blob: fields[1], Stage: stage})
	}
	return t, nil
}

func validateCheckpointTransaction(ctx context.Context, git checkpointGitRunner, t *checkpointTransaction) error {
	if t.Schema != 1 || t.Branch == "" || strings.ContainsAny(t.Branch, "\x00\r\n") || !validLandingHead(t.Worktree) || len(t.Done) == 0 || t.Done[len(t.Done)-1] != t.Stopped {
		return fmt.Errorf("%w: incompatible checkpoint transaction", ErrCheckpointUnsafe)
	}
	if _, err := git(ctx, "check-ref-format", "refs/heads/"+t.Branch); err != nil {
		return errors.Join(ErrCheckpointUnsafe, err)
	}
	heads := append([]string{t.Head, t.Original, t.Onto, t.Stopped}, t.Done...)
	heads = append(heads, t.Todo...)
	for _, pair := range t.Rewritten {
		heads = append(heads, pair[0], pair[1])
	}
	for _, head := range heads {
		if !validLandingHead(head) {
			return ErrCheckpointUnsafe
		}
		if _, err := git(ctx, "cat-file", "-e", head+"^{commit}"); err != nil {
			return errors.Join(ErrCheckpointUnsafe, err)
		}
		if err := checkpointUnpublishedHistory(ctx, git, head); err != nil {
			return err
		}
	}
	for _, source := range append(slices.Clone(t.Done), t.Todo...) {
		if _, err := git(ctx, "merge-base", "--is-ancestor", source, t.Original); err != nil {
			return errors.Join(ErrCheckpointUnsafe, err)
		}
	}
	if _, err := git(ctx, "merge-base", "--is-ancestor", t.Onto, t.Head); err != nil {
		return errors.Join(ErrCheckpointUnsafe, err)
	}
	for _, pair := range t.Rewritten {
		if !slices.Contains(t.Done, pair[0]) || pair[0] == t.Stopped {
			return ErrCheckpointUnsafe
		}
		if _, err := git(ctx, "merge-base", "--is-ancestor", pair[1], t.Head); err != nil {
			return errors.Join(ErrCheckpointUnsafe, err)
		}
	}
	baseline, err := git(ctx, "ls-tree", "-r", "-z", t.Head)
	if err != nil {
		return err
	}
	unchanged := make(map[string]string)
	for _, entry := range strings.Split(strings.TrimSuffix(baseline, "\x00"), "\x00") {
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if ok && len(fields) == 3 {
			unchanged[path] = fields[0] + " " + fields[2]
		}
	}
	seen := make(map[string]map[int]bool)
	for _, entry := range t.Index {
		if entry.Stage < 0 || entry.Stage > 3 || entry.Mode != "100644" && entry.Mode != "100755" || !validLandingHead(entry.Blob) {
			return fmt.Errorf("%w: unsafe index alternative", ErrCheckpointUnsafe)
		}
		stages := seen[entry.Path]
		if stages == nil {
			stages = make(map[int]bool)
			seen[entry.Path] = stages
		}
		if stages[entry.Stage] || entry.Stage == 0 && len(stages) > 0 || entry.Stage > 0 && stages[0] {
			return ErrCheckpointUnsafe
		}
		stages[entry.Stage] = true
		if entry.Stage != 0 || unchanged[entry.Path] != entry.Mode+" "+entry.Blob {
			if !checkpointPathAllowed(entry.Path) || strings.HasPrefix(entry.Path, ".detent/") {
				return fmt.Errorf("%w: unsafe index alternative", ErrCheckpointUnsafe)
			}
			if err := checkpointAlternative(ctx, git, entry.Blob); err != nil {
				return err
			}
		}
	}
	output, err := git(ctx, "ls-tree", "-r", "-z", t.Worktree)
	if err != nil {
		return err
	}
	worktree := make(map[string]string)
	for _, entry := range strings.Split(strings.TrimSuffix(output, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || fields[0] != "100644" && fields[0] != "100755" || fields[1] != "blob" {
			return ErrCheckpointUnsafe
		}
		worktree[path] = fields[0] + " " + fields[2]
		if unchanged[path] != fields[0]+" "+fields[2] {
			if !checkpointPathAllowed(path) || strings.HasPrefix(path, ".detent/") {
				return ErrCheckpointUnsafe
			}
			if err := checkpointAlternative(ctx, git, fields[2]); err != nil {
				return err
			}
		}
	}
	for path, baseline := range unchanged {
		if (!checkpointPathAllowed(path) || strings.HasPrefix(path, ".detent/")) && (!seen[path][0] || worktree[path] != baseline) {
			return fmt.Errorf("%w: excluded baseline path changed", ErrCheckpointUnsafe)
		}
	}
	return nil
}

func checkpointAlternative(ctx context.Context, git checkpointGitRunner, blob string) error {
	kind, err := git(ctx, "cat-file", "-t", blob)
	if err != nil || strings.TrimSpace(kind) != "blob" {
		return errors.Join(ErrCheckpointUnsafe, err)
	}
	size, err := git(ctx, "cat-file", "-s", blob)
	if err != nil {
		return err
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
	if err != nil || bytes > MaxCheckpointRefBytes {
		return errors.Join(ErrCheckpointUnsafe, err)
	}
	data, err := git(ctx, "cat-file", "blob", blob)
	if err != nil {
		return err
	}
	for _, match := range checkpointSecretPattern.FindAllString(data, -1) {
		if !checkpointSyntheticRedactionFixture(match) {
			return fmt.Errorf("%w: possible credential in content alternative", ErrCheckpointUnsafe)
		}
	}
	return nil
}

func (l *LocalGit) pushReworkCheckpoint(ctx context.Context, info Info, issue Issue, previous CheckpointRef, validate func(context.Context) error, releaseSource func()) (_ CheckpointRef, _ bool, err error) {
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil || !paused {
		return previous, false, errors.Join(ErrCheckpointUnsafe, err)
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return previous, false, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	t, err := captureCheckpointTransaction(ctx, info.Path, info.Branch, scratch)
	if err != nil {
		return previous, false, fmt.Errorf("capture transaction: %w", err)
	}
	git := checkpointRefGit(info.Path)
	if err := validateCheckpointTransaction(ctx, git, t); err != nil {
		return previous, false, fmt.Errorf("validate transaction: %w", err)
	}
	data, err := json.Marshal(t)
	if err != nil {
		return previous, false, err
	}
	manifest := filepath.Join(scratch, "transaction.json")
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		return previous, false, err
	}
	output, err := git(ctx, "hash-object", "-w", "--", manifest)
	if err != nil {
		return previous, false, err
	}
	digest := strings.TrimSpace(output)
	if previous.TreeSHA == digest && previous.HeadSHA == t.Head {
		return previous, false, nil
	}
	index := checkpointRefGit(info.Path, "GIT_INDEX_FILE="+filepath.Join(scratch, "transaction-index"))
	if _, err := index(ctx, "read-tree", "--empty"); err != nil {
		return previous, false, err
	}
	if _, err := index(ctx, "update-index", "--add", "--cacheinfo", "100644,"+digest+",transaction.json"); err != nil {
		return previous, false, err
	}
	var alternatives strings.Builder
	for i, entry := range t.Index {
		fmt.Fprintf(&alternatives, "100644 %s\talternatives/%d\x00", entry.Blob, i)
	}
	if _, err := runGitAtWithInput(ctx, info.Path, []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "transaction-index")}, false, alternatives.String(), "update-index", "-z", "--index-info"); err != nil {
		return previous, false, err
	}
	if _, err := index(ctx, "read-tree", "--prefix=worktree/", t.Worktree); err != nil {
		return previous, false, err
	}
	output, err = index(ctx, "write-tree")
	if err != nil {
		return previous, false, err
	}
	wrapper := strings.TrimSpace(output)
	parents := []string{t.Head, t.Original, t.Onto}
	if previous.CommitSHA != "" {
		parents = append(parents, previous.CommitSHA)
	}
	commit, err := l.checkpointCommit(ctx, info.Path, wrapper, parents)
	if err != nil {
		return previous, false, err
	}
	output, err = git(ctx, "rev-list", "--objects", "--disk-usage", commit, "--not", "--remotes")
	if err != nil {
		return previous, false, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil || size > MaxCheckpointRefBytes {
		return previous, false, errors.Join(ErrCheckpointUnsafe, err)
	}
	if !checkpointTransactionMatches(ctx, info.Path, t) {
		return previous, false, fmt.Errorf("%w: rework changed during capture", ErrCheckpointUnsafe)
	}
	if err := validate(ctx); err != nil {
		return previous, false, err
	}
	ref := CheckpointRefName(issue)
	releaseSource()
	if err := publishCheckpointRef(ctx, git, ref, commit, previous.CommitSHA); err != nil {
		return previous, false, err
	}
	return CheckpointRef{Ref: ref, CommitSHA: commit, HeadSHA: t.Head, BaseSHA: t.Onto, TreeSHA: digest, transaction: t}, true, nil
}

func readCheckpointTransaction(ctx context.Context, git checkpointGitRunner, checkpoint *CheckpointRef) (*checkpointTransaction, error) {
	digest, err := git(ctx, "rev-parse", checkpoint.CommitSHA+":transaction.json")
	if err != nil || strings.TrimSpace(digest) != checkpoint.TreeSHA {
		return nil, errors.Join(ErrCheckpointUnsafe, err)
	}
	data, err := git(ctx, "cat-file", "blob", checkpoint.TreeSHA)
	if err != nil || len(data) > MaxCheckpointRefBytes {
		return nil, errors.Join(ErrCheckpointUnsafe, err)
	}
	var transaction checkpointTransaction
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&transaction); err != nil {
		return nil, errors.Join(ErrCheckpointUnsafe, err)
	}
	if err := validateCheckpointTransaction(ctx, git, &transaction); err != nil {
		return nil, err
	}
	worktree, err := git(ctx, "rev-parse", checkpoint.CommitSHA+":worktree")
	if err != nil || strings.TrimSpace(worktree) != transaction.Worktree {
		return nil, errors.Join(ErrCheckpointUnsafe, err)
	}
	return &transaction, nil
}

func checkpointTransactionMatches(ctx context.Context, path string, transaction *checkpointTransaction) (matched bool) {
	scratch, err := changeSourceScratch()
	if err != nil {
		return false
	}
	defer func() { matched = os.RemoveAll(scratch) == nil && matched }()
	actual, err := captureCheckpointTransaction(ctx, path, transaction.Branch, scratch)
	return err == nil && reflect.DeepEqual(actual, transaction)
}

func restoreCheckpointTransaction(ctx context.Context, path string, checkpoint *CheckpointRef) error {
	t := checkpoint.transaction
	git := checkpointRefGit(path)
	if _, err := git(ctx, "-c", "core.hooksPath="+os.DevNull, "read-tree", "-u", "--reset", t.Worktree); err != nil {
		return err
	}
	if _, err := git(ctx, "read-tree", "--empty"); err != nil {
		return err
	}
	var index strings.Builder
	for _, entry := range t.Index {
		fmt.Fprintf(&index, "%s %s %d\t%s\x00", entry.Mode, entry.Blob, entry.Stage, entry.Path)
	}
	if _, err := runGitAtWithInput(ctx, path, []string{"GIT_LITERAL_PATHSPECS=1"}, false, index.String(), "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	if _, err := git(ctx, "update-ref", "--no-deref", "HEAD", t.Head); err != nil {
		return err
	}
	if _, err := git(ctx, "update-ref", "refs/heads/"+t.Branch, t.Original, t.Head); err != nil {
		return err
	}
	dir, err := gitPathFor(ctx, path, "rebase-merge")
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	picks := func(commits []string) string {
		var text strings.Builder
		for _, commit := range commits {
			fmt.Fprintf(&text, "pick %s\n", commit)
		}
		return text.String()
	}
	message, err := git(ctx, "show", "-s", "--format=%B", t.Stopped)
	if err != nil {
		return err
	}
	script, err := checkpointAuthorScript(ctx, git, t.Stopped)
	if err != nil {
		return err
	}
	files := map[string]string{"head-name": "refs/heads/" + t.Branch + "\n", "orig-head": t.Original + "\n", "onto": t.Onto + "\n", "stopped-sha": t.Stopped + "\n", "done": picks(t.Done), "git-rebase-todo": picks(t.Todo), "msgnum": strconv.Itoa(len(t.Done)) + "\n", "end": strconv.Itoa(len(t.Done)+len(t.Todo)) + "\n", "message": message, "author-script": script, "interactive": "", "no-reschedule-failed-exec": ""}
	if t.DropRedundant {
		files["drop_redundant_commits"] = ""
	}
	var rewritten strings.Builder
	for _, pair := range t.Rewritten {
		fmt.Fprintf(&rewritten, "%s %s\n", pair[0], pair[1])
	}
	if rewritten.Len() > 0 {
		files["rewritten-list"] = rewritten.String()
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			return err
		}
	}
	if _, err := git(ctx, "update-ref", "REBASE_HEAD", t.Stopped); err != nil {
		return err
	}
	if !checkpointTransactionMatches(ctx, path, t) {
		return fmt.Errorf("%w: restored transaction differs from checkpoint", ErrCheckpointUnsafe)
	}
	return nil
}

func checkpointAuthorScript(ctx context.Context, git checkpointGitRunner, stopped string) (string, error) {
	author, err := git(ctx, "show", "-s", "--date=raw", "--format=%an%x00%ae%x00%ad", stopped)
	if err != nil {
		return "", err
	}
	fields := strings.Split(strings.TrimSuffix(author, "\n"), "\x00")
	if len(fields) != 3 {
		return "", ErrCheckpointUnsafe
	}
	fields[2] = "@" + fields[2]
	var script strings.Builder
	for i, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE"} {
		fmt.Fprintf(&script, "%s='%s'\n", name, strings.ReplaceAll(fields[i], "'", "'\\''"))
	}
	return script.String(), nil
}
