package gitexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func PushPublication(ctx context.Context, path, repository, refspec string, options ...string) error {
	return withPublicationGit(ctx, path, "", func(scratch string, _ publicationSource) error {
		return pushPublication(ctx, scratch, repository, refspec, options...)
	})
}

func PublicationBranchHead(ctx context.Context, repository, branch string) (string, bool, error) {
	ref := "refs/heads/" + branch
	output, err := ReadPublicationRefs(ctx, repository, false, ref)
	if err != nil {
		return "", false, err
	}
	return parseRemoteBranchHead(output, ref)
}

func FetchPublicationBranch(ctx context.Context, path, remote, repository, branch string) (string, error) {
	return fetchPublicationBranch(ctx, path, remote, repository, branch)
}

func FetchRemoteBranch(ctx context.Context, path, remote, repository, branch string) (string, error) {
	return fetchPublicationBranch(ctx, path, remote, repository, branch)
}

func fetchPublicationBranch(ctx context.Context, path, remote, repository, branch string, extraConfig ...string) (base string, err error) {
	source, err := readPublicationMetadata(ctx, path, "")
	if err != nil {
		return "", err
	}
	objects, err := publicationObjects(source.common)
	if err != nil {
		return "", err
	}
	scratch, err := Scratch()
	if err != nil {
		return "", err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratch); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove publication repository: %w", cleanupErr))
		}
	}()
	if err := InitScratch(ctx, scratch, "--bare", "--object-format="+source.format); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := copyShallowBoundary(source.common, scratch); err != nil {
		return "", err
	}
	isolatedEnv := IsolatedEnvironment()
	seedRefs := []string{"HEAD^{commit}"}
	if remote != "" && branch != "" {
		seedRefs = append([]string{"refs/remotes/" + remote + "/" + branch + "^{commit}"}, seedRefs...)
	}
	var seededTips []string
	seeded := make(map[string]struct{}, len(seedRefs))
	for i, candidate := range seedRefs {
		raw, revErr := ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", candidate)
		if revErr != nil {
			continue
		}
		tip := strings.TrimSpace(raw)
		if !ValidHead(tip) {
			continue
		}
		if _, dup := seeded[tip]; dup {
			continue
		}
		if _, err := withCommandEnvironment(ctx, scratch, isolatedEnv, true, "", RunCommand, "--git-dir="+scratch, "update-ref", fmt.Sprintf("refs/have/%d", i), tip); err != nil {
			return "", fmt.Errorf("seed publication fetch negotiation tip: %w", err)
		}
		seeded[tip] = struct{}{}
		seededTips = append(seededTips, tip)
	}
	remoteFetchArgs := []string{"--git-dir=" + scratch, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "core.alternateRefsCommand=true", "-c", "http.followRedirects=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always", "-c", "protocol.ssh.allow=always"}
	remoteFetchArgs = append(remoteFetchArgs, "fetch", "--no-tags", "--no-write-fetch-head", "--no-auto-maintenance", "--", repository, "refs/heads/"+branch+":refs/heads/target")
	fetchArgs := RemoteArgs(remoteFetchArgs...)
	if _, err := withCommandEnvironment(ctx, path, RemoteFetchEnvironment(), true, "", RunCommand, fetchArgs...); err != nil {
		return "", redactRemoteError(err, repository)
	}
	resolved, err := withCommandEnvironment(ctx, scratch, isolatedEnv, true, "", RunCommand, "--git-dir="+scratch, "rev-parse", "--verify", "refs/heads/target^{commit}")
	if err != nil {
		return "", err
	}
	base = strings.TrimSpace(resolved)
	if !ValidHead(base) {
		return "", errors.New("fetched publication target is not a valid commit")
	}

	if _, existsErr := ReadPublicationSource(ctx, path, "cat-file", "-e", base+"^{commit}"); existsErr != nil {
		var importScratch string
		importScratch, err = Scratch()
		if err != nil {
			return "", err
		}
		defer func() {
			if cleanupErr := os.RemoveAll(importScratch); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove publication import repository: %w", cleanupErr))
			}
		}()
		if err := InitScratch(ctx, importScratch, "--bare", "--object-format="+source.format); err != nil {
			return "", err
		}
		if err := copyShallowBoundary(source.common, importScratch); err != nil {
			return "", err
		}
		importEnv := append(IsolatedEnvironment(), "GIT_OBJECT_DIRECTORY="+objects)
		for i, tip := range seededTips {
			if _, err := withCommandEnvironment(ctx, importScratch, importEnv, true, "", RunCommand, "--git-dir="+importScratch, "update-ref", fmt.Sprintf("refs/have/%d", i), tip); err != nil {
				return "", fmt.Errorf("seed publication import negotiation tip: %w", err)
			}
		}
		importArgs := append([]string{"--git-dir=" + importScratch}, extraConfig...)
		importArgs = append(importArgs, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-write-fetch-head", "--no-auto-maintenance", "--", scratch, "refs/heads/target:refs/heads/imported")
		if _, err := withCommandEnvironment(ctx, importScratch, importEnv, true, "", RunCommand, importArgs...); err != nil {
			return "", err
		}
	}
	return base, nil
}

func FetchCheckpoint(ctx context.Context, path, remote, commitSHA, ref string) (err error) {
	source, err := readPublicationMetadata(ctx, path, "")
	if err != nil {
		return err
	}
	objects, err := publicationObjects(source.common)
	if err != nil {
		return err
	}
	rawURL, err := RunRemoteFetch(ctx, path, "remote", "get-url", remote)
	if err != nil {
		return err
	}
	fetchRepo := strings.TrimSuffix(rawURL, "\n")
	scratch, err := Scratch()
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratch); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove checkpoint scratch repository: %w", cleanupErr))
		}
	}()
	if err := InitScratch(ctx, scratch, "--bare", "--object-format="+source.format); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return err
	}
	if err := copyShallowBoundary(source.common, scratch); err != nil {
		return err
	}
	isolatedEnv := IsolatedEnvironment()
	seedRefs := []string{"HEAD^{commit}"}
	if remote != "" {
		seedRefs = append(seedRefs,
			"refs/remotes/"+remote+"/HEAD^{commit}",
			"refs/remotes/"+remote+"/main^{commit}",
			"refs/remotes/"+remote+"/develop^{commit}",
			"refs/remotes/"+remote+"/master^{commit}",
		)
	}
	var seededTips []string
	seeded := make(map[string]struct{}, len(seedRefs))
	for _, candidate := range seedRefs {
		raw, revErr := ReadPublicationSource(ctx, source.common, "rev-parse", "--verify", "--quiet", candidate)
		if revErr != nil {
			continue
		}
		tip := strings.TrimSpace(raw)
		if !ValidHead(tip) {
			continue
		}
		if _, dup := seeded[tip]; dup {
			continue
		}
		if _, err := withCommandEnvironment(ctx, scratch, isolatedEnv, true, "", RunCommand, "--git-dir="+scratch, "update-ref", fmt.Sprintf("refs/have/%d", len(seededTips)), tip); err != nil {
			return fmt.Errorf("seed checkpoint fetch negotiation tip: %w", err)
		}
		seeded[tip] = struct{}{}
		seededTips = append(seededTips, tip)
	}
	baseArgs := []string{"--git-dir=" + scratch, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "core.alternateRefsCommand=true", "-c", "http.followRedirects=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always", "-c", "protocol.ssh.allow=always", "fetch", "--no-tags", "--no-write-fetch-head", "--no-auto-maintenance", "--", fetchRepo}
	remoteEnv := RemoteFetchEnvironment()
	if err := VerifyRemoteFetchDestination(ctx, path, fetchRepo); err != nil {
		return fmt.Errorf("verify checkpoint remote: %w", err)
	}
	if _, shaErr := withCommandEnvironment(ctx, path, remoteEnv, true, "", RunCommand, RemoteArgs(append(append([]string(nil), baseArgs...), commitSHA+":refs/heads/checkpoint")...)...); shaErr != nil {
		if _, refErr := withCommandEnvironment(ctx, path, remoteEnv, true, "", RunCommand, RemoteArgs(append(append([]string(nil), baseArgs...), "+"+ref+":refs/heads/checkpoint")...)...); refErr != nil {
			return fmt.Errorf("fetch checkpoint %s: %w", ref, errors.Join(redactRemoteError(shaErr, fetchRepo), redactRemoteError(refErr, fetchRepo)))
		}
	}
	importScratch, err := Scratch()
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(importScratch); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove checkpoint import repository: %w", cleanupErr))
		}
	}()
	if err := InitScratch(ctx, importScratch, "--bare", "--object-format="+source.format); err != nil {
		return err
	}
	if err := copyShallowBoundary(source.common, importScratch); err != nil {
		return err
	}
	importEnv := append(IsolatedEnvironment(), "GIT_OBJECT_DIRECTORY="+objects)
	for i, tip := range seededTips {
		if _, err := withCommandEnvironment(ctx, importScratch, importEnv, true, "", RunCommand, "--git-dir="+importScratch, "update-ref", fmt.Sprintf("refs/have/%d", i), tip); err != nil {
			return fmt.Errorf("seed checkpoint import negotiation tip: %w", err)
		}
	}
	importArgs := []string{"--git-dir=" + importScratch, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-write-fetch-head", "--no-auto-maintenance", "--", scratch, "refs/heads/checkpoint:refs/heads/imported"}
	if _, err := withCommandEnvironment(ctx, importScratch, importEnv, true, "", RunCommand, importArgs...); err != nil {
		return err
	}
	return nil
}

func redactRemoteError(err error, rawRepo string) error {
	if err == nil || rawRepo == "" {
		return err
	}
	const redacted = "<redacted-remote>"
	var cmdErr *CommandError
	if errors.As(err, &cmdErr) {
		cloned := *cmdErr
		cloned.Args = make([]string, len(cmdErr.Args))
		for i, arg := range cmdErr.Args {
			cloned.Args[i] = strings.ReplaceAll(arg, rawRepo, redacted)
		}
		cloned.Output = ""
		return &cloned
	}
	return errors.New(strings.ReplaceAll(err.Error(), rawRepo, redacted))
}

func ReadPublicationRefs(ctx context.Context, repository string, symref bool, refs ...string) (string, error) {
	args := []string{"ls-remote"}
	if symref {
		args = append(args, "--symref")
	}
	var output string
	err := withPublicationGit(ctx, "", "", func(scratch string, _ publicationSource) error {
		var err error
		output, err = runPublicationTransport(ctx, scratch, append(append(args, "--", repository), refs...)...)
		return err
	})
	return output, err
}

func ReadRemoteRefs(ctx context.Context, path, repository string, symref bool, refs ...string) (string, error) {
	args := []string{"ls-remote"}
	if symref {
		args = append(args, "--symref")
	}
	var output string
	err := withPublicationGit(ctx, "", "", func(scratch string, _ publicationSource) error {
		options := []string{"--git-dir=" + scratch, "-c", "http.followRedirects=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always", "-c", "protocol.ssh.allow=always"}
		dir := path
		if dir == "" {
			dir = scratch
		}
		var err error
		output, err = withCommandEnvironment(ctx, dir, RemoteFetchEnvironment(), true, "", RunCommand, append(RemoteArgs(options...), append(append(args, "--", repository), refs...)...)...)
		if err != nil {
			return redactRemoteError(err, repository)
		}
		return nil
	})
	return output, err
}

func RunRemoteFetch(ctx context.Context, path string, args ...string) (string, error) {
	return withCommandEnvironment(ctx, path, RemoteFetchEnvironment(), true, "", RunCommand, args...)
}

func RemoteFetchEnvironment() []string {
	source := os.Environ()
	env := make([]string, 0, len(source)+4)
	for _, entry := range source {
		upper := strings.ToUpper(entry)
		if strings.HasPrefix(upper, "GIT_DIR=") ||
			strings.HasPrefix(upper, "GIT_WORK_TREE=") ||
			strings.HasPrefix(upper, "GIT_INDEX_FILE=") ||
			strings.HasPrefix(upper, "GIT_OBJECT_DIRECTORY=") ||
			strings.HasPrefix(upper, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") ||
			strings.HasPrefix(upper, "GIT_COMMON_DIR=") ||
			strings.HasPrefix(upper, "GIT_REPLACE_REF_BASE=") ||
			strings.HasPrefix(upper, "GIT_GRAFT_FILE=") ||
			strings.HasPrefix(upper, "GIT_NAMESPACE=") ||
			strings.HasPrefix(upper, "GIT_SUPER_PREFIX=") ||
			strings.HasPrefix(upper, "GIT_PREFIX=") ||
			strings.HasPrefix(upper, "GIT_SHALLOW_FILE=") ||
			strings.HasPrefix(upper, "GIT_TERMINAL_PROMPT=") ||
			strings.HasPrefix(upper, "GIT_NO_REPLACE_OBJECTS=") ||
			strings.HasPrefix(upper, "GIT_NO_LAZY_FETCH=") ||
			strings.HasPrefix(upper, "GIT_OPTIONAL_LOCKS=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
}

func IsolatedEnvironment() []string {
	source := os.Environ()
	env := make([]string, 0, len(source)+6)
	for _, entry := range source {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
}

func ReadPublicationSource(ctx context.Context, path string, args ...string) (string, error) {
	return withCommandEnvironment(ctx, path, IsolatedEnvironment(), true, "", RunCommand, args...)
}

func WithPublicationObjects(ctx context.Context, path string, use func(string) error) error {
	return withPublicationGit(ctx, path, "", func(scratch string, _ publicationSource) error {
		return use(scratch)
	})
}

func PublicationDiffWithinLimit(ctx context.Context, scratch string, maxBytes int, args ...string) (string, bool, error) {
	env := append(IsolatedEnvironment(), "GIT_ATTR_NOSYSTEM=1")
	return withinLimitEnvironment(ctx, scratch, env, maxBytes, append([]string{"-c", "core.attributesFile=" + os.DevNull, "diff"}, args...)...)
}

func pushPublication(ctx context.Context, scratch, repository, refspec string, options ...string) error {
	args := append([]string{"push", "--no-follow-tags"}, options...)
	_, err := runPublicationTransport(ctx, scratch, append(args, "--", repository, refspec)...)
	return err
}

func runPublicationTransport(ctx context.Context, scratch string, args ...string) (string, error) {
	options := RemoteArgs("--git-dir="+scratch, "-c", "http.followRedirects=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.file.allow=always")
	return ReadPublicationSource(ctx, scratch, append(options, args...)...)
}

type publicationSource struct {
	common string
	gitDir string
	format string
}

func publicationCommonDir(ctx context.Context, path string) (string, error) {
	output, err := ReadPublicationSource(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSuffix(output, "\n"))
}

func readPublicationMetadata(ctx context.Context, path, owner string) (publicationSource, error) {
	output, err := ReadPublicationSource(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir", "--git-dir", "--show-object-format")
	if err != nil {
		return publicationSource{}, err
	}
	fields := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(fields) != 3 || fields[2] != "sha1" && fields[2] != "sha256" {
		return publicationSource{}, errors.New("invalid publication Git metadata")
	}
	common, err := filepath.EvalSymlinks(fields[0])
	if err != nil {
		return publicationSource{}, err
	}
	if owner != "" {
		expected, err := publicationCommonDir(ctx, owner)
		if err != nil {
			return publicationSource{}, err
		}
		if common != expected {
			return publicationSource{}, errors.New("publication checkout is outside the runner source repository")
		}
	}
	dir, format := fields[1], fields[2]
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return publicationSource{}, err
	}
	if dir != common && filepath.Dir(dir) != filepath.Join(common, "worktrees") {
		return publicationSource{}, errors.New("publication checkout Git directory is outside its common directory")
	}
	return publicationSource{common: common, gitDir: dir, format: format}, nil
}

func publicationObjects(common string) (string, error) {
	objects := filepath.Join(common, "objects")
	for _, dir := range []string{objects, filepath.Join(objects, "info"), filepath.Join(objects, "pack")} {
		entry, err := os.Lstat(dir)
		if err != nil {
			return "", fmt.Errorf("inspect publication object store: %w", err)
		}
		if !entry.IsDir() {
			return "", errors.New("publication object store is not a local directory")
		}
	}
	if _, err := os.Lstat(filepath.Join(objects, "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return "", fmt.Errorf("inspect publication object alternates: %w", err)
		}
		return "", errors.New("publication object store uses alternates")
	}
	if strings.ContainsAny(objects, "\r\n") {
		return "", errors.New("publication object store has an invalid path")
	}
	return objects, nil
}

func withPublicationGit(ctx context.Context, path, owner string, use func(string, publicationSource) error) (err error) {
	source := publicationSource{format: "sha1"}
	var objects string
	if path != "" {
		source, err = readPublicationMetadata(ctx, path, owner)
		if err != nil {
			return err
		}
		objects, err = publicationObjects(source.common)
		if err != nil {
			return err
		}
	}
	scratch, err := Scratch()
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratch); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove publication repository: %w", cleanupErr))
		}
	}()
	if err := InitScratch(ctx, scratch, "--bare", "--object-format="+source.format); err != nil {
		return err
	}
	if objects != "" {
		if err := os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
			return err
		}
		if err := copyShallowBoundary(source.common, scratch); err != nil {
			return err
		}
	}
	return use(scratch, source)
}

func copyShallowBoundary(commonDir, scratchDir string) (retErr error) {
	if commonDir == "" {
		return nil
	}
	root, err := os.OpenRoot(commonDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close repository root: %w", err))
		}
	}()
	linfo, err := root.Lstat("shallow")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !linfo.Mode().IsRegular() {
		return errors.New("repository shallow file must be a regular file")
	}
	file, err := os.OpenFile(filepath.Join(commonDir, "shallow"), os.O_RDONLY|shallowOpenFlags, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close source shallow file: %w", err))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(linfo, info) {
		return errors.New("repository shallow file must be a regular file")
	}
	dst, err := os.OpenFile(filepath.Join(scratchDir, "shallow"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(dst, file, make([]byte, 32*1024))
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func VerifyPublicationAncestry(ctx context.Context, path, ancestor, head string) error {
	if !ValidHead(ancestor) || !ValidHead(head) {
		return errors.New("publication ancestry requires exact commit IDs")
	}
	return withPublicationGit(ctx, path, "", func(scratch string, _ publicationSource) error {
		_, err := ReadPublicationSource(ctx, scratch, "merge-base", "--is-ancestor", ancestor, head)
		return err
	})
}

func VerifyPublicationAncestryComplete(ctx context.Context, path, ancestor, head string) (bool, error) {
	if !ValidHead(ancestor) || !ValidHead(head) {
		return false, errors.New("publication ancestry requires exact commit IDs")
	}
	var isAncestor bool
	err := withPublicationGit(ctx, path, "", func(scratch string, _ publicationSource) error {
		_, mergeErr := ReadPublicationSource(ctx, scratch, "merge-base", "--is-ancestor", ancestor, head)
		if mergeErr == nil {
			isAncestor = true
			return nil
		}
		var commandErr *CommandError
		if !errors.As(mergeErr, &commandErr) || commandErr.ExitCode != 1 {
			return mergeErr
		}
		shallow, shallowErr := ReadPublicationSource(ctx, scratch, "rev-parse", "--is-shallow-repository")
		if shallowErr != nil {
			return shallowErr
		}
		if strings.TrimSpace(shallow) == "true" {
			return fmt.Errorf("cannot verify publication ancestry in a shallow repository: %w", mergeErr)
		}
		return nil
	})
	return isAncestor, err
}

func IsolatedReplayWritePaths(ctx context.Context, path, headSHA string, commits []string) ([]string, error) {
	if !ValidHead(headSHA) {
		return nil, errors.New("isolated replay inspection requires an exact head commit ID")
	}
	var writePaths []string
	seen := make(map[string]struct{})
	err := withPublicationGit(ctx, path, "", func(scratch string, _ publicationSource) error {
		read := func(args ...string) (string, error) {
			return ReadPublicationSource(ctx, scratch, args...)
		}
		prevTreeRaw, tErr := read("rev-parse", "--verify", headSHA+"^{tree}")
		if tErr != nil {
			return tErr
		}
		prevTree := strings.TrimSpace(prevTreeRaw)
		var emptyTree string
		for _, commitSHA := range commits {
			if !ValidHead(commitSHA) {
				return errors.New("isolated replay inspection requires exact commit IDs")
			}
			var baseRef string
			parentRaw, pErr := read("rev-parse", "--verify", "--quiet", commitSHA+"^")
			if pErr == nil {
				baseRef = strings.TrimSpace(parentRaw)
			} else {
				var commandErr *CommandError
				if !errors.As(pErr, &commandErr) || commandErr.ExitCode != 1 {
					return pErr
				}
				if emptyTree == "" {
					etRaw, etErr := read("hash-object", "-t", "tree", os.DevNull)
					if etErr != nil {
						return etErr
					}
					emptyTree = strings.TrimSpace(etRaw)
				}
				baseRef = emptyTree
			}
			merged, mErr := read("merge-tree", "--write-tree", "--name-only", "-z", "--merge-base="+baseRef, prevTree, commitSHA)
			var mergedTree string
			if mErr == nil {
				mt, _, _ := strings.Cut(merged, "\x00")
				mergedTree = strings.TrimSpace(mt)
			} else {
				var commandErr *CommandError
				if !errors.As(mErr, &commandErr) || commandErr.ExitCode != 1 {
					return mErr
				}
				mt, _, _ := strings.Cut(commandErr.Output, "\x00")
				mergedTree = strings.TrimSpace(mt)
			}
			if !ValidHead(mergedTree) {
				return errors.New("isolated replay merge tree is unavailable")
			}
			diffRaw, dErr := read("diff-tree", "--no-commit-id", "--no-renames", "--diff-filter=d", "-r", "-z", "--name-only", prevTree, mergedTree)
			if dErr != nil {
				return dErr
			}
			for _, name := range strings.Split(strings.TrimSuffix(diffRaw, "\x00"), "\x00") {
				if name == "" {
					continue
				}
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					writePaths = append(writePaths, name)
				}
			}
			srcDiffRaw, sdErr := read("diff-tree", "--root", "--no-commit-id", "--no-renames", "--diff-filter=d", "-r", "-z", "--name-only", commitSHA)
			if sdErr != nil {
				return sdErr
			}
			for _, name := range strings.Split(strings.TrimSuffix(srcDiffRaw, "\x00"), "\x00") {
				if name == "" {
					continue
				}
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					writePaths = append(writePaths, name)
				}
			}
			prevTree = mergedTree
		}
		return nil
	})
	return writePaths, err
}

func IsolatedMergeTree(ctx context.Context, path, base, branch1, branch2 string, verify func(read func(...string) (string, error), merged string, mergeErr error) error) error {
	if !ValidHead(base) || !ValidHead(branch1) || !ValidHead(branch2) {
		return errors.New("isolated merge-tree requires exact commit IDs")
	}
	return withPublicationGit(ctx, path, "", func(scratch string, _ publicationSource) error {
		read := func(args ...string) (string, error) {
			return ReadPublicationSource(ctx, scratch, args...)
		}
		merged, mergeErr := read("merge-tree", "--write-tree", "--name-only", "-z", "--merge-base="+base, branch1, branch2)
		return verify(read, merged, mergeErr)
	})
}

func IsolatedMergeTreeIndex(ctx context.Context, path, base, branch1, branch2 string, verify func(read func(...string) (string, error), diffIndex func(tree string) (string, error), merged string, mergeErr error) error) error {
	if !ValidHead(base) || !ValidHead(branch1) || !ValidHead(branch2) {
		return errors.New("isolated merge-tree requires exact commit IDs")
	}
	return withPublicationGit(ctx, path, "", func(scratch string, source publicationSource) error {
		read := func(args ...string) (string, error) {
			return ReadPublicationSource(ctx, scratch, args...)
		}
		indexPath := filepath.Join(source.gitDir, "index")
		diffIndex := func(tree string) (string, error) {
			env := append(IsolatedEnvironment(), "GIT_INDEX_FILE="+indexPath)
			return withCommandEnvironment(ctx, scratch, env, true, "", RunCommand, "--git-dir="+scratch, "diff-index", "--cached", "--raw", "--no-renames", "-z", tree)
		}
		merged, mergeErr := read("merge-tree", "--write-tree", "--name-only", "-z", "--merge-base="+base, branch1, branch2)
		return verify(read, diffIndex, merged, mergeErr)
	})
}

func fileHasNonSpaceContent(f *os.File) (bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 && len(bytes.TrimSpace(buf[:n])) > 0 {
			if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
				return false, seekErr
			}
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
				return false, seekErr
			}
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

const isolatedApplyDiagLimit = 8192

func IsolatedApplyTree(ctx context.Context, path, parentSHA string, patchFile *os.File, isMail bool) (string, error) {
	if !ValidHead(parentSHA) {
		return "", errors.New("isolated patch apply requires an exact parent commit ID")
	}
	var tree string
	if err := withPublicationGit(ctx, path, "", func(scratch string, _ publicationSource) (retErr error) {
		env := append(IsolatedEnvironment(), "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
		if _, err := withCommandEnvironment(ctx, scratch, env, true, "", RunCommand, "--git-dir="+scratch, "read-tree", parentSHA); err != nil {
			return err
		}
		applyInput := patchFile
		if isMail {
			if _, err := patchFile.Seek(0, io.SeekStart); err != nil {
				return err
			}
			mailCmd := exec.CommandContext(ctx, "git", "--git-dir=.", "mailinfo", "mailinfo.msg", "mailinfo.patch")
			mailCmd.Dir = scratch
			mailCmd.Env = env
			mailCmd.Stdin = patchFile
			mailErr := &limitedBuffer{limit: isolatedApplyDiagLimit}
			mailCmd.Stdout = io.Discard
			mailCmd.Stderr = mailErr
			if err := RunCommand(ctx, mailCmd); err != nil {
				exitCode := -1
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					exitCode = exitErr.ExitCode()
				}
				return &CommandError{Command: "git", Args: []string{"mailinfo"}, ExitCode: exitCode, Output: mailErr.String(), Err: err}
			}
			extractedFile, err := os.Open(filepath.Join(scratch, "mailinfo.patch"))
			if err != nil {
				return err
			}
			defer func() {
				if err := extractedFile.Close(); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("close extracted patch: %w", err))
				}
			}()
			applyInput = extractedFile
		}
		hasContent, err := fileHasNonSpaceContent(applyInput)
		if err != nil {
			return err
		}
		if hasContent {
			cmd := exec.CommandContext(ctx, "git", "--git-dir=.", "apply", "--cached", "--binary", "--unidiff-zero")
			cmd.Dir = scratch
			cmd.Env = env
			cmd.Stdin = applyInput
			applyErr := &limitedBuffer{limit: isolatedApplyDiagLimit}
			cmd.Stdout = io.Discard
			cmd.Stderr = applyErr
			if err := RunCommand(ctx, cmd); err != nil {
				exitCode := -1
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					exitCode = exitErr.ExitCode()
				}
				return &CommandError{Command: "git", Args: []string{"apply", "--cached", "--binary", "--unidiff-zero"}, ExitCode: exitCode, Output: applyErr.String(), Err: err}
			}
		}
		got, err := withCommandEnvironment(ctx, scratch, env, true, "", RunCommand, "--git-dir="+scratch, "write-tree")
		if err != nil {
			return err
		}
		tree = strings.TrimSpace(got)
		return nil
	}); err != nil {
		return "", err
	}
	return tree, nil
}

func WithPublicationCheckout(ctx context.Context, path, owner string, use func(read func(...string) (string, error), push func(repository, refspec string, options ...string) error) error) error {
	return withPublicationCheckout(ctx, path, owner, false, use)
}

func WithPublicationCheckoutFromHead(ctx context.Context, path, owner string, use func(read func(...string) (string, error)) error) error {
	return withPublicationCheckout(ctx, path, owner, true, func(read func(...string) (string, error), _ func(string, string, ...string) error) error {
		return use(read)
	})
}

func withPublicationCheckout(ctx context.Context, path, owner string, fromHead bool, use func(read func(...string) (string, error), push func(repository, refspec string, options ...string) error) error) error {
	return withPublicationGit(ctx, path, owner, func(scratch string, source publicationSource) error {
		head, err := ReadPublicationSource(ctx, path, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return err
		}
		if !ValidHead(strings.TrimSuffix(head, "\n")) {
			return errors.New("invalid publication checkout head")
		}
		if err := os.WriteFile(filepath.Join(scratch, "HEAD"), []byte(head), 0o600); err != nil {
			return err
		}
		index := filepath.Join(source.gitDir, "index")
		if entry, err := os.Lstat(index); err != nil || !entry.Mode().IsRegular() {
			return errors.Join(errors.New("publication checkout requires a local Git index"), err)
		}
		checkoutReader := func(indexPath string) func(...string) (string, error) {
			env := append(IsolatedEnvironment(), "GIT_INDEX_FILE="+indexPath)
			return func(args ...string) (string, error) {
				options := []string{"--git-dir=" + scratch, "--work-tree=" + path, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.excludesFile=" + filepath.Join(source.common, "info", "exclude")}
				return withCommandEnvironment(ctx, path, env, true, "", RunCommand, append(options, args...)...)
			}
		}
		read := checkoutReader(index)
		if fromHead {
			if _, err := read("diff-index", "--cached", "--quiet", "--ignore-submodules=none", "HEAD", "--"); err != nil {
				return fmt.Errorf("verify staged files against the published tree: %w", err)
			}
			read = checkoutReader(filepath.Join(scratch, "index"))
			if _, err := read("read-tree", "HEAD"); err != nil {
				return fmt.Errorf("prepare index from the published tree: %w", err)
			}
		}
		return use(read, func(repository, refspec string, options ...string) error {
			return pushPublication(ctx, scratch, repository, refspec, options...)
		})
	})
}
