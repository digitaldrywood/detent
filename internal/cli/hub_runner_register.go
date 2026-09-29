package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerServiceName = "detent.runner"

type runnerServiceStarter func(cmd *cobra.Command, configPath string) error

type runnerRegistration struct {
	RunnerID   string                  `json:"runner_id"`
	MachineID  tracker.MachineID       `json:"machine_id"`
	Config     string                  `json:"config"`
	Identity   string                  `json:"identity_file"`
	Created    bool                    `json:"config_created"`
	Projects   []runnerRegisteredCheck `json:"projects"`
	Service    string                  `json:"service,omitempty"`
	NextSteps  []string                `json:"next_steps,omitempty"`
	ServiceRun bool                    `json:"service_started"`
}

type runnerRegisteredCheck struct {
	Name     string            `json:"name"`
	ID       tracker.ProjectID `json:"id"`
	Workdir  string            `json:"workdir"`
	Checkout bool              `json:"checkout"`
}

func newHubRunnerRegisterCommand(version string, lookupEnv func(string) string, startService runnerServiceStarter) *cobra.Command {
	var hubURL, token, organization, name, configPath, workspaceRoot string
	var capacity int
	var service bool
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register this host as a runner with one command from the Enroll dialog",
		Long: "Generates this host's runner identity locally, redeems the one-time enrollment token, writes the runner configuration, and with --service installs and starts it as a background service. " +
			"The token is single-use and expires within 15 minutes; the credential the host generates is sent once and stored only as a hash by the Hub.",
		Example:      `detent hub runner register --url https://hub.detent.build/organizations/org_example --token det_enroll_example --name "Build host" --service`,
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token = strings.TrimSpace(firstNonBlankString(token, lookupEnv("DETENT_RUNNER_ENROLLMENT_TOKEN")))
			if token == "" {
				return errors.New("an enrollment token is required: pass --token or set DETENT_RUNNER_ENROLLMENT_TOKEN")
			}
			base, org, err := runnerHubTarget(hubURL, organization)
			if err != nil {
				return err
			}
			if capacity < 1 {
				return errors.New("--capacity must be at least 1")
			}
			paths, err := resolveRunnerPaths(configPath, workspaceRoot)
			if err != nil {
				return err
			}
			hostname, err := os.Hostname()
			if err != nil {
				return errors.New("host name is unavailable")
			}
			name = strings.TrimSpace(firstNonBlankString(name, hostname))
			if _, err := os.Stat(paths.identity); errors.Is(err, os.ErrNotExist) {
				if _, err := runnerauth.Initialize(paths.identity, base); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			identity, err := hubclient.EnrollRunner(cmd.Context(), paths.identity, org, token, hubclient.Machine{Hostname: hostname, DisplayName: name, Capacity: capacity, Version: firstNonBlankString(version, "dev")})
			if err != nil {
				return err
			}
			projects, err := runnerProjectNames(cmd.Context(), base, paths.identity, identity)
			if err != nil {
				return err
			}
			result := runnerRegistration{RunnerID: identity.RunnerID, MachineID: identity.MachineID, Config: paths.config, Identity: paths.identity}
			for _, project := range projects {
				workdir := filepath.Join(paths.workspaces, project.Name)
				_, statErr := os.Stat(filepath.Join(workdir, ".git"))
				result.Projects = append(result.Projects, runnerRegisteredCheck{Name: project.Name, ID: project.ID, Workdir: workdir, Checkout: statErr == nil})
			}
			config := runnerConfig(base, org, name, capacity, paths, result.Projects)
			result.Created, err = writeRunnerConfig(paths.config, config)
			if err != nil {
				return err
			}
			missing := false
			for _, project := range result.Projects {
				if !project.Checkout {
					missing = true
					result.NextSteps = append(result.NextSteps, fmt.Sprintf("Clone the %s repository into %s", project.Name, project.Workdir))
				}
			}
			start := fmt.Sprintf("detent start --config %s --yes", shellQuote(paths.config))
			switch {
			case service && !missing:
				if err := startService(cmd, paths.config); err != nil {
					return err
				}
				result.ServiceRun = true
				result.Service = runnerServiceName
			case service:
				result.NextSteps = append(result.NextSteps, "Then start the runner service: "+start)
			default:
				result.NextSteps = append(result.NextSteps, "Start the runner: "+start+" (or run it in the foreground: detent --config "+shellQuote(paths.config)+" --headless)")
			}
			return writeRunnerRegistration(cmd, result)
		},
	}
	cmd.Flags().StringVar(&hubURL, "url", "", "the organization's Hub URL from the Enroll dialog")
	cmd.Flags().StringVar(&token, "token", "", "one-time enrollment token (or set DETENT_RUNNER_ENROLLMENT_TOKEN)")
	cmd.Flags().StringVar(&organization, "organization", "", "organization ID, when the URL does not name one")
	cmd.Flags().StringVar(&name, "name", "", "display name shown in the Hub (default: host name)")
	cmd.Flags().IntVar(&capacity, "capacity", 1, "how many work items this runner takes at once")
	cmd.Flags().StringVar(&configPath, "config", "", "runner configuration path (default: ~/.config/detent-runner/global.yaml)")
	cmd.Flags().StringVar(&workspaceRoot, "workspace-root", "", "where project checkouts live (default: ~/detent-runner)")
	cmd.Flags().BoolVar(&service, "service", false, "install and start the runner as a background service ("+runnerServiceName+")")
	return cmd
}

// runnerHubTarget splits the Enroll dialog's URL into the Hub base the runner
// talks to and the organization it enrolls in.
func runnerHubTarget(raw, organization string) (string, tracker.OrganizationID, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", "", errors.New("--url is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("--url must be the organization's Hub URL, for example https://hub.detent.build/organizations/org_example")
	}
	org := strings.TrimSpace(organization)
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == "organizations" {
			if org != "" && org != segments[i+1] {
				return "", "", fmt.Errorf("--organization %s does not match the URL's organization %s", org, segments[i+1])
			}
			org = segments[i+1]
		}
	}
	if !strings.HasPrefix(org, "org_") {
		return "", "", errors.New("the organization is unknown: use the URL from the Enroll dialog or pass --organization")
	}
	return raw, tracker.OrganizationID(org), nil
}

type runnerPaths struct {
	config     string
	identity   string
	workspaces string
}

func resolveRunnerPaths(configPath, workspaceRoot string) (runnerPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return runnerPaths{}, err
	}
	if configPath == "" {
		configPath = filepath.Join(home, ".config", "detent-runner", "global.yaml")
	}
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(home, "detent-runner")
	}
	if !filepath.IsAbs(configPath) || !filepath.IsAbs(workspaceRoot) {
		return runnerPaths{}, errors.New("--config and --workspace-root must be absolute paths")
	}
	identity := filepath.Join(filepath.Dir(configPath), "identity.json")
	if err := runnerPrivateLocation(identity); err != nil {
		return runnerPaths{}, err
	}
	return runnerPaths{config: filepath.Clean(configPath), identity: identity, workspaces: filepath.Clean(workspaceRoot)}, nil
}

type runnerProject struct {
	Name string
	ID   tracker.ProjectID
}

func runnerProjectNames(ctx context.Context, base, identityPath string, identity runnerauth.Identity) ([]runnerProject, error) {
	client, err := hubclient.New(hubclient.Config{URL: base, IdentityFile: identityPath})
	if err != nil {
		return nil, err
	}
	projects := make([]runnerProject, 0, len(identity.ProjectIDs))
	used := map[string]bool{}
	for _, id := range identity.ProjectIDs {
		native, err := client.Native(identity.OrganizationID, id)
		if err != nil {
			return nil, err
		}
		project, err := native.Project(ctx)
		if err != nil {
			return nil, fmt.Errorf("read project %s: %w", id, err)
		}
		name := runnerProjectSlug(project.Name, id)
		if used[name] {
			name = runnerProjectSlug(project.Name+"-"+string(id), id)
		}
		used[name] = true
		projects = append(projects, runnerProject{Name: name, ID: id})
	}
	return projects, nil
}

// runnerProjectSlug is the local project ID and checkout directory name.
func runnerProjectSlug(name string, id tracker.ProjectID) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-.")
	if slug == "" {
		return string(id)
	}
	return slug
}

type runnerConfigFile struct {
	APIVersion   string              `yaml:"apiVersion"`
	Kind         string              `yaml:"kind"`
	InstanceName string              `yaml:"instance_name"`
	ServiceName  string              `yaml:"service_name"`
	Port         int                 `yaml:"port"`
	Client       runnerConfigClient  `yaml:"client"`
	Global       runnerConfigGlobal  `yaml:"global"`
	Projects     []runnerConfigEntry `yaml:"projects"`
}

type runnerConfigClient struct {
	HubURL         string            `yaml:"hub_url"`
	IdentityFile   string            `yaml:"identity_file"`
	OrganizationID string            `yaml:"organization_id"`
	NativeProjects map[string]string `yaml:"native_projects"`
	DisplayName    string            `yaml:"display_name"`
	Capacity       int               `yaml:"capacity"`
}

type runnerConfigGlobal struct {
	MaxConcurrentAgents int    `yaml:"max_concurrent_agents"`
	Scheduling          string `yaml:"scheduling"`
}

type runnerConfigEntry struct {
	ID       string `yaml:"id"`
	Workflow string `yaml:"workflow"`
	Workdir  string `yaml:"workdir"`
	Weight   int    `yaml:"weight"`
	Priority int    `yaml:"priority"`
}

func runnerConfig(base string, org tracker.OrganizationID, name string, capacity int, paths runnerPaths, projects []runnerRegisteredCheck) runnerConfigFile {
	config := runnerConfigFile{
		APIVersion:   "detent/v1",
		Kind:         "GlobalConfig",
		InstanceName: name,
		ServiceName:  runnerServiceName,
		Client: runnerConfigClient{
			HubURL:         base,
			IdentityFile:   paths.identity,
			OrganizationID: string(org),
			NativeProjects: map[string]string{},
			DisplayName:    name,
			Capacity:       capacity,
		},
		Global:   runnerConfigGlobal{MaxConcurrentAgents: capacity, Scheduling: "weighted"},
		Projects: []runnerConfigEntry{},
	}
	for _, project := range projects {
		config.Client.NativeProjects[project.Name] = string(project.ID)
		config.Projects = append(config.Projects, runnerConfigEntry{ID: project.Name, Workflow: filepath.Join(project.Workdir, "WORKFLOW.md"), Workdir: project.Workdir, Weight: 1, Priority: 3})
	}
	return config
}

// writeRunnerConfig creates the runner configuration once. An existing file
// is the operator's and is never rewritten.
func writeRunnerConfig(path string, config runnerConfigFile) (bool, error) {
	if !globalconfig.ValidServiceName(config.ServiceName) {
		return false, errors.New("runner service name is invalid")
	}
	body, err := yaml.Marshal(config)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	header := "# Detent runner configuration written by `detent hub runner register`.\n# The Hub assigns work for the projects below; each workdir is that project's repository checkout.\n"
	_, writeErr := file.Write(append([]byte(header), body...))
	return true, errors.Join(writeErr, file.Close())
}

func writeRunnerRegistration(cmd *cobra.Command, result runnerRegistration) error {
	output, err := OutputForCommand(cmd)
	if err != nil {
		return err
	}
	return output.Write(func(w io.Writer) error {
		lines := []string{
			fmt.Sprintf("Registered runner %s (machine %s).", result.RunnerID, result.MachineID),
		}
		if result.Created {
			lines = append(lines, "Wrote "+result.Config)
		} else {
			lines = append(lines, "Kept the existing "+result.Config)
		}
		for _, project := range result.Projects {
			state := "checkout found"
			if !project.Checkout {
				state = "no checkout yet"
			}
			lines = append(lines, fmt.Sprintf("Project %s (%s): %s, %s", project.Name, project.ID, project.Workdir, state))
		}
		if result.ServiceRun {
			lines = append(lines, "Started the "+result.Service+" service.")
		}
		for _, step := range result.NextSteps {
			lines = append(lines, "Next: "+step)
		}
		_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
		return err
	}, result)
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
