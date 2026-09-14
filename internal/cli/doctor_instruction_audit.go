package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type doctorInstructionFile struct {
	path  string
	text  string
	bytes int
}

var doctorReadDirective = regexp.MustCompile(`(?i)\b(read|follow|consult|load|review)\b`)
var doctorInstructionPath = regexp.MustCompile(`[[:alnum:]_./-]+\.[[:alnum:]_]+\b|\bMakefile\b`)
var doctorQuotedInstructionPath = regexp.MustCompile("`([^`[:space:]]+)`")
var doctorDirectReadPath = regexp.MustCompile(`(?i)\b(read|follow|consult|load|review)\s+(?:the\s+)?(?:file\s+)?$`)

// This is a static estimate: repository instructions at the configured workdir,
// the effective workflow prompt, and literal files named by read directives.
// Worker-specific template values and dynamically constructed paths are unknown.
func doctorInstructionFiles(ctx context.Context, root, prompt string) ([]doctorInstructionFile, []string) {
	var files []doctorInstructionFile
	var problems []string
	root, err := expandDoctorWorkspacePath(root)
	if err != nil {
		return []doctorInstructionFile{{"WORKFLOW.md (effective prompt)", prompt, len(prompt)}}, []string{err.Error()}
	}
	gitRoot, gitErr := doctorWorkflowSourceGit(ctx, root, "rev-parse", "--show-toplevel")
	var dirs []string
	if gitErr != nil {
		problems = append(problems, "resolve instruction repository: "+gitErr.Error())
		// Codex still reads the current directory outside a Git repository.
		dirs = []string{root}
	} else {
		gitRoot = strings.TrimSpace(gitRoot)
		for dir := root; ; dir = filepath.Dir(dir) {
			dirs = append(dirs, dir)
			if dir == gitRoot {
				break
			}
			if filepath.Dir(dir) == dir {
				problems = append(problems, "workdir is outside instruction repository")
				dirs = []string{root}
				break
			}
		}
	}
	slices.Reverse(dirs)
	seen := map[string]bool{}
	remaining := 32 * 1024
	for _, dir := range dirs {
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
			path := filepath.Join(dir, name)
			data, readErr := os.ReadFile(path)
			if os.IsNotExist(readErr) {
				continue
			}
			if readErr != nil {
				problems = append(problems, path+": "+readErr.Error())
				break
			}
			if len(strings.TrimSpace(string(data))) == 0 {
				continue
			}
			count := min(len(data), remaining)
			files = append(files, doctorInstructionFile{path, string(data[:count]), count})
			seen[path] = true
			remaining -= count
			break
		}
	}
	files = append(files, doctorInstructionFile{"WORKFLOW.md (effective prompt)", prompt, len(prompt)})
	// Scan newly discovered files too; seen deduplicates references and cycles.
	for i := 0; i < len(files); i++ {
		for _, line := range strings.Split(files[i].text, "\n") {
			if !doctorReadDirective.MatchString(line) {
				continue
			}
			names := doctorInstructionPath.FindAllString(line, -1)
			for _, match := range doctorQuotedInstructionPath.FindAllStringSubmatchIndex(line, -1) {
				name := line[match[2]:match[3]]
				if strings.ContainsAny(name, "${}:=*|;&") {
					continue
				}
				path := name
				if !filepath.IsAbs(path) {
					path = filepath.Join(root, path)
				}
				info, statErr := os.Stat(path)
				// Quoted lane names and Git refs are not file reads. Extensionless
				// paths need either an existing file or an immediate read directive.
				if statErr == nil && !info.IsDir() || doctorDirectReadPath.MatchString(line[:match[0]]) {
					names = append(names, name)
				}
			}
			for _, name := range names {
				if name == "WORKFLOW.md" || strings.Contains(filepath.Base(name), "..") {
					continue
				}
				path := name
				if !filepath.IsAbs(path) {
					path = filepath.Join(root, path)
				}
				path = filepath.Clean(path)
				if seen[path] {
					continue
				}
				seen[path] = true
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					problems = append(problems, path+": "+readErr.Error())
					continue
				}
				files = append(files, doctorInstructionFile{path, string(data), len(data)})
			}
		}
	}
	return files, problems
}

func checkDoctorInstructionBudget(id string, files []doctorInstructionFile, problems []string) doctorCheck {
	check := doctorCheck{Name: "Project " + id + " instruction_budget", Status: doctorOK}
	total := 0
	var details []string
	for _, file := range files {
		total += file.bytes
		details = append(details, fmt.Sprintf("%s: %d bytes", file.path, file.bytes))
	}
	switch {
	case total > 48*1024:
		check.Status = doctorFail
	case total > 24*1024 || len(problems) > 0:
		check.Status = doctorWarn
	}
	check.Detail = fmt.Sprintf("%d bytes estimated instruction load (AGENTS chain capped at 32768 bytes); %s", total, strings.Join(append(details, problems...), "; "))
	check.Hint = "Static estimate excludes dynamic template values and nonliteral read paths; reduce repeated instructions above 24 KiB (failure above 48 KiB)."
	return check
}

func checkDoctorGateInstructionConflict(id, command string, files []doctorInstructionFile, problems []string) doctorCheck {
	check := doctorCheck{Name: "Project " + id + " gate_instruction_conflict", Status: doctorOK}
	if strings.TrimSpace(command) == "" {
		check.Detail = "gate.run is unset"
		return check
	}
	mention := regexp.MustCompile("(?:^|[^[:alnum:]_-])" + regexp.QuoteMeta(command) + "(?:$|[^[:alnum:]_-])")
	forbidden := regexp.MustCompile(`(?i)\b(do not|don't|never|forbid|forbids|forbidden|must not|skip|except|unless)\b`)
	required := regexp.MustCompile(`(?i)\b(run|must|required|require|requires|mandatory|gate is)\b`)
	var requires, forbids []string
	for _, file := range files {
		var need, ban bool
		for _, line := range strings.Split(file.text, "\n") {
			if !mention.MatchString(line) {
				continue
			}
			if forbidden.MatchString(line) {
				ban = true
			} else if required.MatchString(line) {
				need = true
			}
		}
		if need {
			requires = append(requires, file.path)
		}
		if ban {
			forbids = append(forbids, file.path)
		}
	}
	if len(requires) > 1 || len(requires) > 0 && len(forbids) > 0 || len(problems) > 0 {
		check.Status = doctorWarn
	}
	check.Detail = fmt.Sprintf("%q required in [%s]; forbidden/conditional in [%s]", command, strings.Join(requires, ", "), strings.Join(forbids, ", "))
	if len(problems) > 0 {
		check.Detail += "; incomplete evidence: " + strings.Join(problems, "; ")
	}
	check.Hint = "Keep the gate requirement in one instruction file; this line-based heuristic includes conditional prohibitions."
	// A fast Go gate often coexists with instructions about its full gate.
	// Audit that explicit full-gate command too so contradictory make check
	// requirements do not disappear when gate.run changes to make check-fast.
	if command == "make check-fast" {
		full := checkDoctorGateInstructionConflict(id, "make check", files, nil)
		if full.Status == doctorWarn {
			check.Status = doctorWarn
			check.Detail += "; full gate: " + full.Detail
		}
	}
	return check
}

func checkDoctorWorkflowSourceDrift(ctx context.Context, id string, project globalconfig.Project, cfg workflowconfig.Config, deps doctorDeps) doctorCheck {
	check := doctorCheck{Name: "Project " + id + " workflow_source_drift", Status: doctorOK}
	root := projectSourceRoot(project, cfg)
	if project.Workdir != "" {
		root = project.Workdir
	}
	root, err := expandDoctorWorkspacePath(root)
	if err != nil {
		check.Status = doctorWarn
		check.Detail = err.Error()
		return check
	}
	path := strings.TrimSpace(project.Workflow)
	if filepath.IsAbs(path) || strings.HasPrefix(path, "~/") {
		path, err = expandDoctorWorkspacePath(path)
	} else {
		path = filepath.Join(root, path)
	}
	if err != nil {
		check.Status = doctorWarn
		check.Detail = err.Error()
		return check
	}
	working, err := os.ReadFile(path)
	if err != nil {
		check.Status = doctorWarn
		check.Detail = err.Error()
		return check
	}
	ref := strings.TrimSpace(project.WorkflowRef)
	if ref == "" {
		if deps.githubRepositoryInfo == nil || !doctorTrackerUsesGitHubReads(cfg.Tracker.Kind) {
			check.Status = doctorWarn
			check.Detail = fmt.Sprintf("working tree %d bytes; tracker default branch unavailable", len(working))
			return check
		}
		info, infoErr := deps.githubRepositoryInfo(ctx, cfg, cfg.Tracker.Repository)
		if infoErr != nil || info.DefaultBranch == "" {
			check.Status = doctorWarn
			check.Detail = fmt.Sprintf("working tree %d bytes; tracker default branch unavailable: %v", len(working), infoErr)
			return check
		}
		branch, branchErr := doctorWorkflowSourceGit(ctx, root, "branch", "--show-current")
		if branchErr != nil || strings.TrimSpace(branch) != info.DefaultBranch {
			check.Status = doctorWarn
		}
		check.Detail = fmt.Sprintf("workflow_ref unset; checkout branch %q; tracker default branch %q; ", strings.TrimSpace(branch), info.DefaultBranch)
		ref = "refs/remotes/origin/" + info.DefaultBranch
	}
	// External unpinned workflows are valid. Compare their file in its own
	// checkout while retaining the configured workdir's branch evidence above.
	comparisonRoot := root
	if project.WorkflowRef == "" {
		owner, ownerErr := doctorWorkflowSourceGit(ctx, filepath.Dir(path), "rev-parse", "--show-toplevel")
		if ownerErr == nil {
			comparisonRoot = strings.TrimSpace(owner)
		}
	}
	relative, relativeErr := filepath.Rel(comparisonRoot, path)
	if relativeErr != nil || !filepath.IsLocal(relative) {
		check.Status = doctorWarn
		check.Detail += fmt.Sprintf("working tree %d bytes; %s size unavailable: workflow is outside ref checkout", len(working), ref)
		return check
	}
	reference, err := doctorWorkflowSourceGit(ctx, comparisonRoot, "show", ref+":"+filepath.ToSlash(relative))
	check.Detail += fmt.Sprintf("working tree %d bytes; %s", len(working), ref)
	if err != nil {
		check.Status = doctorWarn
		check.Detail += " size unavailable: " + err.Error()
		return check
	}
	check.Detail += fmt.Sprintf(" %d bytes", len(reference))
	if project.WorkflowRef != "" && string(working) != reference {
		check.Status = doctorWarn
		check.Detail += "; WORKFLOW.md differs"
	}
	return check
}
