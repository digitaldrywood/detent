package releaseaudit

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

type GitSource struct {
	repository string
	commit     string
	scratch    string
}

func (s *GitSource) Sandbox(ctx context.Context, scratch string) (Sandbox, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("prlimit"); err != nil {
		return nil, err
	}
	target, err := os.MkdirTemp(scratch, "pinned-audit-target-")
	if err != nil {
		return nil, err
	}
	tree, err := s.git(ctx, "ls-tree", "-r", "-z", s.commit)
	if err != nil {
		return nil, err
	}
	var links [][2]string
	total := 0
	for _, entry := range strings.Split(strings.TrimSuffix(string(tree), "\x00"), "\x00") {
		metadata, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || fields[1] != "blob" || !filepath.IsLocal(name) || strings.ContainsAny(name, "\\\x00") {
			return nil, errors.New("pinned release audit tree cannot be safely materialized")
		}
		content, err := s.Read(ctx, name)
		if err != nil {
			return nil, err
		}
		total += len(content)
		if total > 128<<20 {
			return nil, errors.New("pinned release audit tree exceeds its materialization allowance")
		}
		filename := filepath.Join(target, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			return nil, err
		}
		if fields[0] == "120000" {
			links = append(links, [2]string{filename, string(content)})
			continue
		}
		mode := os.FileMode(0o644)
		if fields[0] == "100755" {
			mode = 0o755
		} else if fields[0] != "100644" {
			return nil, errors.New("pinned release audit tree has an unsupported file mode")
		}
		if err := os.WriteFile(filename, content, mode); err != nil {
			return nil, err
		}
	}
	for _, link := range links {
		if err := os.Symlink(link[1], link[0]); err != nil {
			return nil, err
		}
	}
	return NewSandbox(target, scratch)
}

func NewGitSource(ctx context.Context, repository, commit, scratch string) (*GitSource, error) {
	if len(commit) != 40 || !filepath.IsAbs(repository) || !filepath.IsAbs(scratch) {
		return nil, errors.New("release audit source requires an absolute repository, scratch and full commit hash")
	}
	if _, err := hex.DecodeString(commit); err != nil {
		return nil, errors.New("release audit source commit is invalid")
	}
	source := &GitSource{repository: repository, commit: commit, scratch: scratch}
	output, err := source.git(ctx, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil || strings.TrimSpace(string(output)) != commit {
		return nil, errors.New("release audit pinned commit is unavailable")
	}
	return source, nil
}

func (s *GitSource) Commit() string { return s.commit }

func (s *GitSource) git(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git")
	command.Args = append(command.Args, arguments...)
	command.Dir = s.repository
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.scratch, "TMPDIR=" + s.scratch, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0"}
	output := &boundedOutput{limit: maxDocumentBytes}
	command.Stdout = output
	err := command.Run()
	if err != nil {
		return nil, fmt.Errorf("read pinned release audit source: %w", err)
	}
	return output.buffer.Bytes(), nil
}

func (s *GitSource) Paths(ctx context.Context) ([]string, error) {
	output, err := s.git(ctx, "ls-tree", "-r", "--name-only", "-z", s.commit)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00"), nil
}

func (s *GitSource) Read(ctx context.Context, name string) ([]byte, error) {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\x00\\") {
		return nil, errors.New("release audit source path is invalid")
	}
	return s.git(ctx, "cat-file", "blob", s.commit+":"+name)
}
