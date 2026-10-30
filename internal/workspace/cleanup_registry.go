package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var nativeWorkspaceKeyPattern = regexp.MustCompile(`^(.+)-(prj_[a-z0-9]+)_([1-9][0-9]*)-([a-f0-9]{12})(?:-review-(.+))?$`)

const (
	cleanupOwnershipSchema      = 1
	cleanupOwnershipRegistryDir = ".detent/cleanup-ownership"
)

type cleanupOwnershipRecord struct {
	Schema          int    `json:"schema"`
	Path            string `json:"path"`
	Key             string `json:"key"`
	ProjectID       string `json:"project_id,omitempty"`
	IssueID         string `json:"issue_id,omitempty"`
	Identifier      string `json:"identifier,omitempty"`
	Branch          string `json:"branch,omitempty"`
	SourceCommonDir string `json:"source_common_dir"`
	CleanupStarted  bool   `json:"cleanup_started,omitempty"`
	// WorkspaceSession records that the worktree belongs to a workspace
	// session, so a later pass does not have to infer it from a branch name.
	WorkspaceSession bool   `json:"workspace_session,omitempty"`
	ReviewHeadSHA    string `json:"review_head_sha,omitempty"`
}

type cleanupInventory struct {
	unowned  int
	records  []cleanupOwnershipRecord
	recorded map[string]bool
}

type cleanupInventoryScanner struct {
	mu        sync.Mutex
	directory *os.File
	cursor    string
	next      string
	entries   []fs.DirEntry
}

func (s *cleanupInventoryScanner) page(root, cursor string, limit int) ([]fs.DirEntry, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		return nil, cursor, nil
	}
	if s.entries != nil && cursor == s.cursor {
		return s.entries, s.next, nil
	}
	if s.entries != nil && cursor != s.next {
		if err := s.directory.Close(); err != nil {
			return nil, cursor, err
		}
		s.directory, s.entries = nil, nil
	}
	if s.directory == nil {
		directory, err := os.Open(root)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", nil
		}
		if err != nil {
			return nil, cursor, err
		}
		s.directory = directory
	}
	entries, err := s.directory.ReadDir(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		closeErr := s.directory.Close()
		s.directory, s.entries = nil, nil
		return nil, cursor, errors.Join(err, closeErr)
	}
	if len(entries) == 0 {
		err := s.directory.Close()
		s.directory, s.entries = nil, nil
		return nil, "", err
	}
	s.cursor, s.next, s.entries = cursor, entries[len(entries)-1].Name(), entries
	return s.entries, s.next, nil
}

func (l *LocalGit) CleanupIssues(ctx context.Context, cursor string, limit int) ([]Issue, string, error) {
	if batch, ok := ctx.Value(cleanupBatchKey{}).(*cleanupBatch); ok {
		batch.inventories[l.cleanupInventory] = cleanupInventory{}
	}
	entries, next, err := l.cleanupInventory.page(l.root, cursor, limit)
	if err != nil {
		return nil, cursor, err
	}
	inventory := cleanupInventory{recorded: make(map[string]bool)}
	var issues []Issue
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, cursor, ctx.Err()
		}
		if !entry.IsDir() || entry.Name() == ".detent" {
			continue
		}
		path, err := l.workspacePath(entry.Name())
		if err != nil {
			continue
		}
		relative := cleanupOwnershipRecordRelativePath(path)
		record, err := l.readOwnershipRecord(relative)
		if err == nil {
			if !l.validOwnershipRecord(ctx, relative, record) {
				inventory.unowned++
				continue
			}
			inventory.recorded[path] = true
		} else if errors.Is(err, fs.ErrNotExist) {
			if !l.isSourceWorktree(ctx, path) {
				continue
			}
			record = cleanupOwnershipRecord{Path: path, Key: entry.Name()}
			branch, branchErr := runGitAt(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
			if branchErr == nil {
				record.Branch = strings.TrimSpace(branch)
			} else {
				for _, candidate := range []string{workspaceSessionBranchName(record.Key), l.branchName(Issue{ReviewVersionID: "review"}, record.Key)} {
					exists, err := l.branchExists(ctx, candidate)
					if err != nil {
						return nil, cursor, err
					}
					if exists {
						record.Branch = candidate
						break
					}
				}
			}
			l.inferNativeOwnership(&record)
		} else {
			continue
		}
		inventory.records = append(inventory.records, record)
		if record.Identifier != "" {
			issues = append(issues, record.issue())
		}
	}
	if batch, ok := ctx.Value(cleanupBatchKey{}).(*cleanupBatch); ok {
		batch.inventories[l.cleanupInventory] = inventory
	}
	return issues, next, nil
}

func (l *LocalGit) recordCleanupOwnership(ctx context.Context, info Info, issue Issue, isDir bool) error {
	if isDir && !l.isSourceWorktree(ctx, info.Path) && l.isGitWorkspace(ctx, info.Path) {
		return fmt.Errorf("refusing to record cleanup ownership for git workspace not managed by source: %s", info.Path)
	}
	path, err := validateWorkspacePath(l.root, info.Path)
	if err != nil {
		return err
	}
	sourceCommonDir, err := gitCommonDir(ctx, l.sourceRoot)
	if err != nil {
		return fmt.Errorf("resolve cleanup ownership source: %w", err)
	}
	record := cleanupOwnershipRecord{
		Schema:           cleanupOwnershipSchema,
		Path:             path,
		Key:              info.Key,
		ProjectID:        strings.TrimSpace(issue.ProjectID),
		IssueID:          strings.TrimSpace(issue.ID),
		Identifier:       strings.TrimSpace(issue.Identifier),
		Branch:           strings.TrimSpace(info.Branch),
		SourceCommonDir:  sourceCommonDir,
		WorkspaceSession: sessionOwned(info, issue),
	}
	if record.Key == "" {
		record.Key = filepath.Base(path)
	}
	if record.Key != filepath.Base(path) {
		return fmt.Errorf("cleanup ownership key %q does not match workspace path %q", record.Key, path)
	}
	if record.Identifier != "" && !issueOwnsKey(issue, record.Key) {
		return fmt.Errorf("cleanup ownership issue does not match workspace key %q", record.Key)
	}
	previous, err := l.readOwnershipRecord(cleanupOwnershipRecordRelativePath(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read previous cleanup ownership: %w", err)
	}
	if issue.ReviewVersionID != "" && issue.Source != nil && issue.Source.Version.ID == issue.ReviewVersionID && issue.Source.Version.Source != nil {
		record.ReviewHeadSHA = issue.Source.Version.HeadSHA
	} else if err == nil && l.validOwnershipRecord(ctx, cleanupOwnershipRecordRelativePath(path), previous) {
		record.ReviewHeadSHA = previous.ReviewHeadSHA
	}
	if err := l.writeOwnershipRecord(record); err != nil {
		return fmt.Errorf("record cleanup ownership: %w", err)
	}
	return nil
}

func (l *LocalGit) writeOwnershipRecord(record cleanupOwnershipRecord) (returnErr error) {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, root.Close())
	}()
	if err := root.MkdirAll(cleanupOwnershipRegistryDir, 0o700); err != nil {
		return err
	}
	recordPath := cleanupOwnershipRecordRelativePath(record.Path)
	temporaryPath, err := cleanupOwnershipTemporaryPath(recordPath)
	if err != nil {
		return err
	}
	file, err := root.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, file.Close())
		}
		if cleanupErr := root.Remove(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	if err := root.Rename(temporaryPath, recordPath); err != nil {
		return err
	}
	return nil
}

func cleanupOwnershipTemporaryPath(recordPath string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return recordPath + ".tmp-" + hex.EncodeToString(suffix[:]), nil
}

func cleanupOwnershipRecordRelativePath(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return filepath.Join(cleanupOwnershipRegistryDir, hex.EncodeToString(sum[:])[:24]+".json")
}

func (l *LocalGit) removeOwnershipRecord(path string) (returnErr error) {
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, root.Close())
	}()
	err = root.Remove(cleanupOwnershipRecordRelativePath(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove cleanup ownership record: %w", err)
	}
	return nil
}

func (l *LocalGit) cleanupOwnershipRecorded(ctx context.Context, path string) (bool, error) {
	record, err := l.readOwnershipRecord(cleanupOwnershipRecordRelativePath(path))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return l.validOwnershipRecord(ctx, cleanupOwnershipRecordRelativePath(path), record), nil
}

func (l *LocalGit) readOwnershipRecord(relativePath string) (_ cleanupOwnershipRecord, returnErr error) {
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return cleanupOwnershipRecord{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, root.Close())
	}()
	info, err := root.Lstat(relativePath)
	if err != nil {
		return cleanupOwnershipRecord{}, err
	}
	if !info.Mode().IsRegular() {
		return cleanupOwnershipRecord{}, fmt.Errorf("cleanup ownership record is not a regular file: %s", relativePath)
	}
	data, err := root.ReadFile(relativePath)
	if err != nil {
		return cleanupOwnershipRecord{}, err
	}
	var decoded cleanupOwnershipRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		return cleanupOwnershipRecord{}, err
	}
	return decoded, nil
}

func (l *LocalGit) validOwnershipRecord(ctx context.Context, relativePath string, record cleanupOwnershipRecord) bool {
	if record.Schema != cleanupOwnershipSchema || record.Path == "" || record.Key == "" || record.SourceCommonDir == "" {
		return false
	}
	path, err := validateWorkspacePath(l.root, record.Path)
	if err != nil || path != filepath.Clean(record.Path) || filepath.Base(path) != record.Key {
		return false
	}
	if cleanupOwnershipRecordRelativePath(path) != relativePath {
		return false
	}
	if record.Identifier != "" && !issueOwnsKey(record.issue(), record.Key) {
		return false
	}
	sourceCommonDir, err := gitCommonDir(ctx, l.sourceRoot)
	return err == nil && sourceCommonDir == filepath.Clean(record.SourceCommonDir)
}

func (record cleanupOwnershipRecord) issue() Issue {
	issue := Issue{ProjectID: record.ProjectID, ID: record.IssueID, Identifier: record.Identifier, WorkspaceSession: record.WorkspaceSession}
	for _, key := range []string{issueKey(issue), legacyIssueKey(issue)} {
		if version, ok := strings.CutPrefix(record.Key, key+"-review-"); ok && version != "" {
			issue.ReviewVersionID = version
			break
		}
	}
	return issue
}

func (l *LocalGit) ReconcileResiduals(ctx context.Context, activeIssues []Issue) (ReconcileResult, error) {
	activeKeys := make(map[string]struct{}, len(activeIssues))
	for _, issue := range activeIssues {
		activeKeys[issueKey(issue)] = struct{}{}
		activeKeys[legacyIssueKey(issue)] = struct{}{}
	}
	result := ReconcileResult{}
	inventory, err := l.cleanupRecords(ctx)
	if err != nil {
		return result, err
	}
	result.UnownedSkipped = inventory.unowned
	var reconcileErrors []error
	for _, record := range inventory.records {
		if cleanupBatchExhausted(ctx) {
			break
		}
		if _, active := activeKeys[record.Key]; active || nativeOwnershipActive(record, activeIssues) {
			result.ActiveSkipped++
			continue
		}
		removed, reconcileErr := l.reconcileWorkspace(WithCleanupAdmission(ctx, ctx), record, inventory.recorded[record.Path], &result)
		if reconcileErr != nil {
			result.Failures = append(result.Failures, CleanupFailure{Path: record.Path, Error: reconcileErr.Error()})
			if errors.Is(reconcileErr, ErrWorkspacePreserved) {
				// Retention is a completed inspection, not a failed sweep. Keep
				// path evidence without causing the caller to retry every refresh.
				result.PreservedSkipped++
				continue
			}
			reconcileErrors = append(reconcileErrors, reconcileErr)
			continue
		}
		if removed {
			result.CompletedPaths = append(result.CompletedPaths, record.Path)
		}
	}
	return result, errors.Join(reconcileErrors...)
}

func (l *LocalGit) cleanupRecords(ctx context.Context) (cleanupInventory, error) {
	if batch, ok := ctx.Value(cleanupBatchKey{}).(*cleanupBatch); ok {
		if inventory, ok := batch.inventories[l.cleanupInventory]; ok {
			return inventory, nil
		}
	}
	inventory := cleanupInventory{recorded: make(map[string]bool)}
	entries, err := os.ReadDir(filepath.Join(l.root, cleanupOwnershipRegistryDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return inventory, fmt.Errorf("read cleanup ownership registry: %w", err)
	}
	for _, entry := range entries {
		relative := filepath.Join(cleanupOwnershipRegistryDir, entry.Name())
		record, err := l.readOwnershipRecord(relative)
		if err == nil && l.validOwnershipRecord(ctx, relative, record) {
			inventory.recorded[record.Path] = true
			inventory.records = append(inventory.records, record)
		} else {
			inventory.unowned++
		}
	}
	unrecorded, err := l.unrecordedWorkspaces(ctx, inventory.recorded)
	inventory.records = append(inventory.records, unrecorded...)
	return inventory, err
}

func (l *LocalGit) ownsUnrecordedBranch(record cleanupOwnershipRecord) bool {
	return record.Branch == autoBranchPrefix+strings.ToLower(record.Key) || record.Branch == workspaceSessionBranchName(record.Key) ||
		strings.Contains(record.Key, "-review-") && record.Branch == l.branchName(Issue{ReviewVersionID: "review"}, record.Key)
}

func (l *LocalGit) reconcileWorkspace(ctx context.Context, record cleanupOwnershipRecord, recorded bool, result *ReconcileResult) (bool, error) {
	if !recorded && !l.ownsUnrecordedBranch(record) {
		result.UnownedSkipped++
		return false, nil
	}
	if sessionHeld(record.Path) {
		result.ActiveSkipped++
		return false, nil
	}
	exists, _, err := pathExists(record.Path)
	if err != nil {
		return false, err
	}
	if exists {
		pids, err := scanOwnedWorkspaceProcessIDs(ctx, record.Path, l.scanWorkspacePaths)
		if err != nil {
			return false, err
		}
		if len(pids) > 0 {
			result.ActiveSkipped++
			return false, nil
		}
	}
	info := Info{Path: record.Path, Key: record.Key, Branch: record.Branch}
	issue := record.issue()
	issue.WorkspaceSession = issue.WorkspaceSession || sessionOwned(info, Issue{})
	cleaned, err := l.cleanupWorkspace(ctx, info, issue)
	result.Removed += cleaned.Worktrees
	return err == nil, err
}

func (l *LocalGit) unrecordedWorkspaces(ctx context.Context, recorded map[string]bool) ([]cleanupOwnershipRecord, error) {
	output, err := l.runGit(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("discover unrecorded workspaces: %w", err)
	}
	var records []cleanupOwnershipRecord
	for index, entry := range strings.Split(output, "\x00\x00") {
		if index == 0 {
			continue
		}
		var path, branch string
		for _, field := range strings.Split(entry, "\x00") {
			if value, ok := strings.CutPrefix(field, "worktree "); ok {
				path = value
			}
			if value, ok := strings.CutPrefix(field, "branch refs/heads/"); ok {
				branch = value
			}
		}
		if path == "" {
			continue
		}
		// The source repository also lists manually managed worktrees. Scope
		// discovery before validating candidates, so an unrelated worktree
		// cannot prevent cleanup under our root. Resolve only the parent here:
		// validateWorkspacePath must still reject a symlink at the candidate.
		parent, err := canonicalExistingPath(filepath.Dir(path))
		if err != nil || parent != l.root {
			continue
		}
		path, err = validateWorkspacePath(l.root, path)
		if err != nil {
			return nil, err
		}
		// Git can print forward slashes on Windows while ownership records
		// contain the canonical path returned by validateWorkspacePath.
		if recorded[path] || filepath.Dir(path) != l.root {
			continue
		}
		exists, _, err := pathExists(path)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		key := filepath.Base(path)
		if branch == "" {
			// A workspace session worktree sits on a detached head_sha, so
			// the worktree listing names no branch for it. Its auto-branch is
			// what says the worktree is ours to reclaim.
			session := workspaceSessionBranchName(key)
			owned, err := l.branchExists(ctx, session)
			if err != nil {
				return nil, fmt.Errorf("discover workspace session branch: %w", err)
			}
			if owned {
				branch = session
			}
		}
		record := cleanupOwnershipRecord{Path: path, Key: key, Branch: branch}
		l.inferNativeOwnership(&record)
		records = append(records, record)
	}
	return records, nil
}

func (l *LocalGit) inferNativeOwnership(record *cleanupOwnershipRecord) {
	if !l.ownsUnrecordedBranch(*record) {
		return
	}
	if parts := nativeWorkspaceKeyPattern.FindStringSubmatch(record.Key); len(parts) == 6 {
		issue := Issue{ProjectID: parts[1], Identifier: parts[2] + "#" + parts[3], ReviewVersionID: parts[5]}
		if issueOwnsKey(issue, record.Key) {
			record.ProjectID, record.Identifier = issue.ProjectID, issue.Identifier
		}
	}
}

func issueOwnsKey(issue Issue, key string) bool {
	return issueKey(issue) == key || nativeProjectID(issue) != "" && legacyIssueKey(issue) == key
}

func nativeOwnershipActive(record cleanupOwnershipRecord, issues []Issue) bool {
	review := record.issue().ReviewVersionID != ""
	for _, issue := range issues {
		if (!review || issue.CleanupInUse) && nativeProjectID(issue) != "" && (record.IssueID == "" || record.IssueID == issue.ID) && record.Identifier == issue.Identifier {
			return true
		}
	}
	return false
}
