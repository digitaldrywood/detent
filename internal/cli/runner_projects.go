package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
)

func readRunnerRuntimeConfig(ctx context.Context, path string) (globalconfig.Config, error) {
	cfg, err := globalconfig.Read(path)
	if err != nil {
		return cfg, err
	}
	return resolveRunnerProjects(ctx, cfg)
}

func resolveRunnerProjects(ctx context.Context, cfg globalconfig.Config) (globalconfig.Config, error) {
	if cfg.Client.IdentityFile == "" || cfg.WorkspaceRoot == "" {
		return cfg, nil
	}
	settings := cfg.Client.Normalized()
	client, err := hubclient.New(hubclient.Config{URL: settings.URL, IdentityFile: settings.IdentityFile, HTTPClient: &http.Client{Timeout: settings.RequestTimeout()}})
	if err != nil {
		return cfg, err
	}
	identity, err := client.RunnerIdentity(ctx)
	if err != nil {
		return cfg, fmt.Errorf("read runner's allowed projects: %w", err)
	}
	if string(identity.OrganizationID) != settings.OrganizationID {
		return cfg, errors.New("runner organization does not match enrolled identity")
	}
	cfg.Projects = []globalconfig.Project{}
	cfg.Client.NativeProjects = map[string]string{}
	ids := slices.Clone(identity.ProjectIDs)
	slices.Sort(ids)
	for _, id := range ids {
		name := string(id)
		cfg.Client.NativeProjects[name] = string(id)
		native, err := client.Native(identity.OrganizationID, id)
		if err != nil {
			return cfg, err
		}
		remote, err := native.Project(ctx)
		if err != nil {
			slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
			continue
		}
		if remote.ID != id || remote.OrganizationID != identity.OrganizationID {
			return cfg, errors.New("Hub returned an unexpected project identity")
		}
		workdir, err := runnerProjectCheckout(ctx, cfg.WorkspaceRoot, name, remote.Repository)
		if err != nil {
			slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
			continue
		}
		if _, err := os.Stat(filepath.Join(workdir, ".git")); errors.Is(err, os.ErrNotExist) {
			if err := cloneRunnerProject(ctx, remote.Repository, workdir); err != nil {
				slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
				continue
			}
		} else if err != nil {
			slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
			continue
		}
		if retainedName := filepath.Base(workdir); retainedName != name {
			if _, exists := cfg.Client.NativeProjects[retainedName]; !exists {
				delete(cfg.Client.NativeProjects, name)
				name = retainedName
				cfg.Client.NativeProjects[name] = string(id)
			}
		}
		cfg.Projects = append(cfg.Projects, globalconfig.Project{ID: name, Workflow: filepath.Join(workdir, "WORKFLOW.md"), WorkflowRef: "origin/HEAD", Workdir: workdir, Weight: 1, GlobalCache: cfg.Global.Cache.Normalized()})
	}
	return cfg, ctx.Err()
}

func runnerProjectCheckout(ctx context.Context, root, name, repository string) (string, error) {
	target := filepath.Join(root, name)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return target, nil
	}
	if err != nil {
		return "", err
	}
	var matches []string
	for _, entry := range entries {
		candidate := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err != nil {
			continue
		}
		remote, err := exec.CommandContext(ctx, "git", "-C", candidate, "config", "--get", "remote.origin.url").Output()
		if err != nil {
			continue
		}
		linked, _ := doctorGitHubRepositoryFromRemoteURL(strings.TrimSpace(string(remote)))
		if repository != "" && strings.EqualFold(linked, repository) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple runner checkouts for %s require selecting the retained source", repository)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		return "", fmt.Errorf("runner checkout %s does not match Cloud repository %s", target, repository)
	}
	return target, nil
}

func cloneRunnerProject(ctx context.Context, repository, workdir string) error {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || slices.Contains(parts, "") || strings.ContainsAny(repository, "\\: \t\r\n?#%") || strings.HasPrefix(parts[0], "-") || strings.HasPrefix(parts[1], "-") || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return errors.New("Cloud project requires a linked owner/repository to clone")
	}
	if err := os.MkdirAll(filepath.Dir(workdir), 0o700); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "git", "clone", "--", "git@github.com:"+repository+".git", workdir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
