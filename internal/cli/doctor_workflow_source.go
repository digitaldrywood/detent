package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type doctorWorkflowSourcePolicyFunc func(context.Context, string, globalconfig.Project, string, string) doctorCheck

func checkDoctorWorkflowSourcePolicy(
	ctx context.Context,
	projectID string,
	project globalconfig.Project,
	cfg workflowconfig.Config,
	sourceRoot string,
	deps doctorDeps,
) (doctorCheck, bool) {
	if deps.workflowSourcePolicy == nil {
		return doctorCheck{}, false
	}
	workflowSourceRoot := sourceRoot
	if strings.TrimSpace(project.Workdir) != "" {
		expandedWorkdir, err := expandDoctorWorkspacePath(project.Workdir)
		if err != nil {
			return doctorCheck{
				Name:   "Project " + projectID + " workflow source policy",
				Status: doctorFail,
				Detail: fmt.Sprintf("project %s workflow source workdir could not be resolved: %v", projectID, err),
				Hint:   "Fix projects[].workdir, then rerun detent doctor.",
			}, true
		}
		workflowSourceRoot = expandedWorkdir
	}
	if strings.TrimSpace(project.WorkflowRef) != "" {
		return deps.workflowSourcePolicy(ctx, projectID, project, workflowSourceRoot, ""), true
	}

	name := "Project " + projectID + " workflow source policy"
	repository := ""
	if deps.gitRemoteURL != nil {
		if remote, err := deps.gitRemoteURL(ctx, workflowSourceRoot); err == nil {
			repository, _ = doctorGitHubRepositoryFromRemoteURL(remote)
		}
	}
	if repository == "" {
		repository = strings.TrimSpace(cfg.Tracker.Repository)
	}
	if repository == "" {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "project " + projectID + " omits workflow_ref and reads merge policy from a mutable working tree; source repository default branch could not be resolved",
			Hint:   "Configure a GitHub origin remote and set projects[].workflow_ref to its default remote-tracking branch.",
		}, true
	}
	if deps.githubRepositoryInfo == nil {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "project " + projectID + " omits workflow_ref and reads merge policy from a mutable working tree; GitHub repository metadata is unavailable",
			Hint:   "Rerun detent doctor with GitHub repository access, then set projects[].workflow_ref to the default remote-tracking branch.",
		}, true
	}
	info, err := deps.githubRepositoryInfo(ctx, cfg, repository)
	if err != nil {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: fmt.Sprintf("project %s omits workflow_ref and reads merge policy from a mutable working tree; read %s default branch: %v", projectID, repository, err),
			Hint:   "Fix GitHub repository access, then rerun detent doctor.",
		}, true
	}
	defaultBranch := strings.TrimSpace(info.DefaultBranch)
	if defaultBranch == "" {
		return doctorCheck{
			Name:   name,
			Status: doctorWarn,
			Detail: "project " + projectID + " omits workflow_ref and reads merge policy from a mutable working tree; GitHub returned no default branch for " + repository,
			Hint:   "Set a repository default branch, then rerun detent doctor.",
		}, true
	}
	return deps.workflowSourcePolicy(ctx, projectID, project, workflowSourceRoot, defaultBranch), true
}

func defaultDoctorWorkflowSourcePolicy(
	ctx context.Context,
	projectID string,
	project globalconfig.Project,
	sourceRoot string,
	defaultBranch string,
) doctorCheck {
	if strings.TrimSpace(project.WorkflowRef) != "" {
		return checkDoctorWorkflowRefFreshness(ctx, projectID, project, sourceRoot)
	}
	return checkDoctorMutableWorkflowSource(ctx, projectID, project, sourceRoot, defaultBranch)
}

func checkDoctorWorkflowRefFreshness(ctx context.Context, projectID string, project globalconfig.Project, sourceRoot string) doctorCheck {
	name := "Project " + projectID + " workflow source policy"
	ref := strings.TrimSpace(project.WorkflowRef)
	localRevision, err := doctorWorkflowSourceGit(ctx, sourceRoot, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: fmt.Sprintf("project %s workflow_ref %s could not be resolved: %v", projectID, ref, err), Hint: "Fetch the configured workflow ref, then rerun detent doctor."}
	}
	localRevision = strings.TrimSpace(localRevision)
	fullRef, err := doctorWorkflowSourceGit(ctx, sourceRoot, "rev-parse", "--symbolic-full-name", ref)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("project %s workflow_ref %s resolved to %s but its remote counterpart could not be identified: %v", projectID, ref, doctorShortRevision(localRevision), err), Hint: "Use a remote-tracking workflow_ref such as origin/main so doctor can verify freshness."}
	}
	remote, branch, ok := doctorWorkflowRemoteBranch(strings.TrimSpace(fullRef))
	if !ok {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("project %s workflow_ref %s resolves to %s but is not a remote-tracking branch; remote freshness was not verified", projectID, ref, doctorShortRevision(localRevision)), Hint: "Use a remote-tracking workflow_ref such as origin/main so doctor can verify freshness."}
	}
	remoteRevision, err := doctorWorkflowRemoteRevision(ctx, sourceRoot, remote, branch)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorWarn, Detail: fmt.Sprintf("project %s workflow_ref %s resolves to %s; read remote counterpart %s/%s: %v", projectID, ref, doctorShortRevision(localRevision), remote, branch, err), Hint: "Restore remote access and fetch the configured ref, then rerun detent doctor."}
	}
	if localRevision != remoteRevision {
		return doctorCheck{
			Name:   name,
			Status: doctorFail,
			Detail: fmt.Sprintf("project %s workflow_ref %s is stale at %s; remote counterpart %s/%s is %s", projectID, ref, doctorShortRevision(localRevision), remote, branch, doctorShortRevision(remoteRevision)),
			Hint:   fmt.Sprintf("Fetch %s/%s so the configured workflow ref advances, then rerun detent doctor.", remote, branch),
		}
	}
	return doctorCheck{
		Name:   name,
		Status: doctorOK,
		Detail: fmt.Sprintf("project %s workflow_ref %s is fresh at %s and ignores the working-tree branch", projectID, ref, doctorShortRevision(localRevision)),
	}
}

func checkDoctorMutableWorkflowSource(
	ctx context.Context,
	projectID string,
	project globalconfig.Project,
	sourceRoot string,
	defaultBranch string,
) doctorCheck {
	name := "Project " + projectID + " workflow source policy"
	status := doctorWarn
	details := []string{"project " + projectID + " omits workflow_ref and reads merge policy from a mutable working tree"}
	branch, err := doctorWorkflowSourceGit(ctx, sourceRoot, "branch", "--show-current")
	if err != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: details[0] + "; inspect checkout branch: " + err.Error(), Hint: "Repair the source checkout, then rerun detent doctor."}
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		head, headErr := doctorWorkflowSourceGit(ctx, sourceRoot, "rev-parse", "--short", "HEAD")
		if headErr != nil {
			return doctorCheck{Name: name, Status: doctorFail, Detail: details[0] + "; inspect detached HEAD: " + headErr.Error(), Hint: "Repair the source checkout, then rerun detent doctor."}
		}
		details = append(details, "checkout is detached at "+strings.TrimSpace(head))
	} else if branch != defaultBranch {
		details = append(details, fmt.Sprintf("checkout branch %s differs from default branch %s", branch, defaultBranch))
	} else {
		details = append(details, fmt.Sprintf("checkout branch %s matches default branch %s", branch, defaultBranch))
	}

	configPath, relativeConfigPath, err := doctorWorkflowConfigPaths(project, sourceRoot)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorFail, Detail: strings.Join(append(details, err.Error()), "; "), Hint: "Keep WORKFLOW.md and detent.yaml inside the configured project workdir."}
	}
	workingConfig, err := os.ReadFile(configPath)
	if err != nil {
		detail := "read effective detent.yaml at " + configPath + ": " + err.Error()
		if errors.Is(err, os.ErrNotExist) {
			detail = "effective detent.yaml is missing at " + configPath
		}
		return doctorCheck{Name: name, Status: doctorFail, Detail: strings.Join(append(details, detail), "; "), Hint: "Restore a readable detent.yaml, then rerun detent doctor."}
	}

	remoteRevision, remoteErr := doctorWorkflowRemoteRevision(ctx, sourceRoot, "origin", defaultBranch)
	trackingRef := "refs/remotes/origin/" + defaultBranch
	trackingRevision, trackingErr := doctorWorkflowSourceGit(ctx, sourceRoot, "rev-parse", "--verify", trackingRef+"^{commit}")
	trackingRevision = strings.TrimSpace(trackingRevision)
	comparisonRevision := trackingRevision
	comparisonRef := "origin/" + defaultBranch
	if remoteErr != nil {
		details = append(details, fmt.Sprintf("remote default branch freshness could not be verified: %v", remoteErr))
	} else if trackingErr != nil {
		if doctorWorkflowObjectExists(ctx, sourceRoot, remoteRevision) {
			comparisonRevision = remoteRevision
			comparisonRef = remoteRevision
		} else {
			status = doctorFail
			details = append(details, fmt.Sprintf("local %s is unavailable and remote %s is not present locally", comparisonRef, doctorShortRevision(remoteRevision)))
		}
	} else if trackingRevision != remoteRevision {
		status = doctorFail
		details = append(details, fmt.Sprintf("local %s is stale at %s; remote %s is %s", comparisonRef, doctorShortRevision(trackingRevision), defaultBranch, doctorShortRevision(remoteRevision)))
		if doctorWorkflowObjectExists(ctx, sourceRoot, remoteRevision) {
			comparisonRevision = remoteRevision
			comparisonRef = remoteRevision
		} else {
			details = append(details, "remote default-branch commit is not available locally")
		}
	} else {
		details = append(details, fmt.Sprintf("local %s matches its remote at %s", comparisonRef, doctorShortRevision(remoteRevision)))
	}
	if comparisonRevision == "" {
		return doctorCheck{Name: name, Status: doctorFail, Detail: strings.Join(details, "; "), Hint: fmt.Sprintf("Fetch origin/%s, then rerun detent doctor.", defaultBranch)}
	}

	defaultConfig, err := doctorWorkflowSourceGit(ctx, sourceRoot, "show", comparisonRevision+":"+relativeConfigPath)
	if err != nil {
		status = doctorFail
		details = append(details, fmt.Sprintf("read detent.yaml from %s: %v", comparisonRef, err))
	} else if !bytes.Equal(workingConfig, []byte(defaultConfig)) {
		status = doctorFail
		details = append(details, "effective detent.yaml differs from "+comparisonRef)
	} else {
		details = append(details, "effective detent.yaml matches "+comparisonRef)
	}

	return doctorCheck{
		Name:   name,
		Status: status,
		Detail: strings.Join(details, "; "),
		Hint:   fmt.Sprintf("Set projects[].workflow_ref to origin/%s after fetching that ref, then rerun detent doctor.", defaultBranch),
	}
}

func doctorWorkflowConfigPaths(project globalconfig.Project, sourceRoot string) (string, string, error) {
	workflowPath := strings.TrimSpace(project.Workflow)
	if filepath.IsAbs(workflowPath) || workflowPath == "~" || strings.HasPrefix(workflowPath, "~/") {
		expanded, err := expandDoctorWorkspacePath(workflowPath)
		if err != nil {
			return "", "", fmt.Errorf("resolve workflow path: %w", err)
		}
		workflowPath = expanded
	} else {
		workflowPath = filepath.Join(sourceRoot, workflowPath)
	}
	configPath := workflowconfig.DefinitionPath(workflowPath)
	relativePath, err := filepath.Rel(sourceRoot, configPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve detent.yaml path relative to source root: %w", err)
	}
	if relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("effective detent.yaml %s is outside source root %s", configPath, sourceRoot)
	}
	return configPath, filepath.ToSlash(relativePath), nil
}

func doctorWorkflowRemoteBranch(fullRef string) (string, string, bool) {
	value, ok := strings.CutPrefix(strings.TrimSpace(fullRef), "refs/remotes/")
	if !ok {
		return "", "", false
	}
	remote, branch, ok := strings.Cut(value, "/")
	if !ok || strings.TrimSpace(remote) == "" || strings.TrimSpace(branch) == "" {
		return "", "", false
	}
	return remote, branch, true
}

func doctorWorkflowRemoteRevision(ctx context.Context, sourceRoot string, remote string, branch string) (string, error) {
	ref := "refs/heads/" + branch
	output, err := doctorWorkflowSourceGit(ctx, sourceRoot, "ls-remote", "--exit-code", remote, ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("remote %s branch %s returned no revision", remote, branch)
}

func doctorWorkflowObjectExists(ctx context.Context, sourceRoot string, revision string) bool {
	if strings.TrimSpace(revision) == "" {
		return false
	}
	_, err := doctorWorkflowSourceGit(ctx, sourceRoot, "cat-file", "-e", revision+"^{commit}")
	return err == nil
}

func doctorWorkflowSourceGit(ctx context.Context, sourceRoot string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, doctorCommandTimeout)
	defer cancel()

	commandArgs := append([]string{"-C", sourceRoot}, args...)
	cmd := exec.CommandContext(commandCtx, "git", commandArgs...) // #nosec G204 -- doctor passes configured refs and paths as fixed git arguments without a shell.
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}

func doctorShortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}
