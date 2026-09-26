package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Per-file diffs (decisions section 18.5). GitDiffFrom answers one patch blob,
// which is what a prompt wants; the stored attempt diff wants the same change
// split per file with counts, so the hub can serve a file list without
// re-parsing a patch on every read.
//
// Everything here runs against a copy of the worktree's index, exactly as
// GitDiffFrom does, so an intent-to-add of the untracked files never touches
// the index the worker is using.

// FileDiff is one changed file between a base commit and the worktree.
type FileDiff struct {
	Path      string
	OldPath   string
	Status    string
	Additions int
	Deletions int
	Binary    bool
	Patch     string
}

// FileDiffs is the whole change, per file.
type FileDiffs struct {
	// BaseSHA is the commit the diff was taken against: the merge base of the
	// requested base ref and HEAD when there is one, else HEAD itself.
	BaseSHA string
	// HeadSHA is the worktree's HEAD at the time of the diff.
	HeadSHA string
	Files   []FileDiff
	// Truncated reports that the patch output exceeded maxBytes. The file
	// list and its counts are still complete; the patches are absent.
	Truncated bool
}

// File diff statuses, matching the vocabulary the hub stores.
const (
	FileDiffAdded    = "added"
	FileDiffModified = "modified"
	FileDiffDeleted  = "deleted"
	FileDiffRenamed  = "renamed"
)

// GitFileDiffs computes the worktree's change against baseRef, split per file.
// An empty baseRef diffs against HEAD. maxBytes bounds the patch output as a
// whole; zero asks for counts only.
func GitFileDiffs(ctx context.Context, workspacePath string, baseRef string, maxBytes int) (FileDiffs, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return FileDiffs{}, errors.New("workspace path is required")
	}
	if maxBytes < 0 {
		return FileDiffs{}, errors.New("max bytes must be greater than or equal to 0")
	}
	if _, err := os.Stat(workspacePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileDiffs{}, fmt.Errorf("%w: %s: %w", ErrMissingWorkspace, workspacePath, err)
		}
		return FileDiffs{}, fmt.Errorf("stat workspace path: %w", err)
	}
	if err := ensureGitInfoExcludes(ctx, workspacePath, detentHandoffDiffExcludes); err != nil {
		return FileDiffs{}, err
	}
	indexPath, err := gitIndexPath(ctx, workspacePath)
	if err != nil {
		return FileDiffs{}, err
	}
	tempIndex, cleanup, err := copyGitIndex(indexPath)
	if err != nil {
		return FileDiffs{}, err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}
	if _, err := runGitAtWithEnv(ctx, workspacePath, env, "add", "--intent-to-add", "--", "."); err != nil {
		return FileDiffs{}, fmt.Errorf("git add intent to add: %w", err)
	}
	diffBase := gitDiffBase(ctx, workspacePath, baseRef)
	result := FileDiffs{BaseSHA: gitResolve(ctx, workspacePath, diffBase), HeadSHA: gitResolve(ctx, workspacePath, "HEAD")}

	// --find-renames is passed explicitly on every command below so the file
	// list, the counts and the patch agree even where diff.renames is off in
	// the repository's configuration.
	numstat, err := runGitAtWithEnv(ctx, workspacePath, env, "diff", "--no-ext-diff", "--find-renames", "--numstat", "-z", diffBase)
	if err != nil {
		return FileDiffs{}, fmt.Errorf("git diff numstat: %w", err)
	}
	files, err := parseGitNumstat(numstat)
	if err != nil {
		return FileDiffs{}, err
	}
	statuses, err := runGitAtWithEnv(ctx, workspacePath, env, "diff", "--no-ext-diff", "--find-renames", "--name-status", "-z", diffBase)
	if err != nil {
		return FileDiffs{}, fmt.Errorf("git diff name-status: %w", err)
	}
	applyGitNameStatus(files, statuses)
	result.Files = files
	if maxBytes == 0 || len(files) == 0 {
		result.Truncated = maxBytes == 0 && len(files) != 0
		return result, nil
	}
	patch, truncated, err := gitDiffArgsWithinLimit(ctx, workspacePath, env, maxBytes,
		"diff", "--no-ext-diff", "--find-renames", diffBase)
	if err != nil {
		return FileDiffs{}, err
	}
	if truncated {
		result.Truncated = true
		return result, nil
	}
	attachGitPatches(result.Files, splitGitPatch(patch))
	return result, nil
}

// gitResolve reads a revision's object name, answering an empty string when
// there is none. An unborn branch has no HEAD, which is a state a fresh
// worktree is legitimately in, so it is not an error.
func gitResolve(ctx context.Context, workspacePath, revision string) string {
	output, err := runGitAt(ctx, workspacePath, "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// parseGitNumstat reads `git diff --numstat -z`. Each record is
// "<added>\t<removed>\t<path>\0", and a rename or copy instead emits
// "<added>\t<removed>\t\0<old>\0<new>\0". A binary file reports "-" for both
// counts.
func parseGitNumstat(output string) ([]FileDiff, error) {
	fields := strings.Split(output, "\x00")
	files := []FileDiff{}
	for index := 0; index < len(fields); index++ {
		record := fields[index]
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("git diff numstat record %q is malformed", record)
		}
		file := FileDiff{Status: FileDiffModified}
		if parts[0] == "-" && parts[1] == "-" {
			file.Binary = true
		} else {
			added, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, fmt.Errorf("git diff numstat additions %q: %w", parts[0], err)
			}
			removed, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("git diff numstat deletions %q: %w", parts[1], err)
			}
			file.Additions, file.Deletions = added, removed
		}
		if parts[2] != "" {
			file.Path = parts[2]
			files = append(files, file)
			continue
		}
		if index+2 >= len(fields) || fields[index+1] == "" || fields[index+2] == "" {
			return nil, fmt.Errorf("git diff numstat rename record %q is truncated", record)
		}
		file.OldPath, file.Path = fields[index+1], fields[index+2]
		file.Status = FileDiffRenamed
		index += 2
		files = append(files, file)
	}
	return files, nil
}

// applyGitNameStatus overlays `git diff --name-status -z` onto the numstat
// list. Both commands report the same files in the same order, so the status
// letters are zipped positionally; a record that names a different path is
// ignored rather than misapplied.
func applyGitNameStatus(files []FileDiff, output string) {
	fields := strings.Split(output, "\x00")
	position := 0
	for index := 0; index < len(fields) && position < len(files); index++ {
		letter := fields[index]
		if letter == "" {
			continue
		}
		paths := 1
		if letter[0] == 'R' || letter[0] == 'C' {
			paths = 2
		}
		if index+paths >= len(fields) {
			return
		}
		path := fields[index+paths]
		index += paths
		if path != files[position].Path {
			continue
		}
		switch letter[0] {
		case 'A':
			files[position].Status = FileDiffAdded
		case 'D':
			files[position].Status = FileDiffDeleted
		case 'R':
			files[position].Status = FileDiffRenamed
			files[position].OldPath = fields[index-1]
		case 'C':
			// A copy has no old content to show as removed, so it reads as an
			// addition rather than as a rename that never happened.
			files[position].Status = FileDiffAdded
		default:
			files[position].Status = FileDiffModified
		}
		position++
	}
}

// splitGitPatch cuts a unified diff into one section per file. Every file
// section begins at a line starting with "diff --git ", which is the only
// marker git guarantees, so the sections are taken in order rather than by
// parsing the header's paths: a path with a space in it is ambiguous in that
// header and the order is not.
func splitGitPatch(patch string) []string {
	const marker = "diff --git "
	if patch == "" {
		return nil
	}
	var sections []string
	start := -1
	offset := 0
	for offset < len(patch) {
		end := strings.IndexByte(patch[offset:], '\n')
		lineEnd := len(patch)
		if end >= 0 {
			lineEnd = offset + end + 1
		}
		if strings.HasPrefix(patch[offset:lineEnd], marker) {
			if start >= 0 {
				sections = append(sections, patch[start:offset])
			}
			start = offset
		}
		offset = lineEnd
	}
	if start >= 0 {
		sections = append(sections, patch[start:])
	}
	return sections
}

// attachGitPatches assigns each patch section to its file. The two lists are
// produced by the same diff with the same options, so they describe the same
// files in the same order; a length mismatch means one of them is not what it
// claims, and no patch is attached rather than the wrong one.
func attachGitPatches(files []FileDiff, sections []string) {
	if len(files) != len(sections) {
		return
	}
	for index := range files {
		files[index].Patch = sections[index]
	}
}
