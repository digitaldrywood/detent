package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	}
	return use(scratch, source)
}

func WithPublicationCheckout(ctx context.Context, path, owner string, use func(read func(...string) (string, error), push func(repository, refspec string) error) error) error {
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
		env := append(IsolatedEnvironment(), "GIT_INDEX_FILE="+index)
		read := func(args ...string) (string, error) {
			options := []string{"--git-dir=" + scratch, "--work-tree=" + path, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.excludesFile=" + filepath.Join(source.common, "info", "exclude")}
			return withCommandEnvironment(ctx, path, env, true, "", RunCommand, append(options, args...)...)
		}
		return use(read, func(repository, refspec string) error {
			return pushPublication(ctx, scratch, repository, refspec)
		})
	})
}
