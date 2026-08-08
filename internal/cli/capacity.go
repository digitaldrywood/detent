package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type capacityClearResult struct {
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Cleared int    `json:"cleared"`
}

func newCapacityCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "capacity",
		Short:   "Manage provider capacity outages",
		Example: "detent capacity clear --scope codex",
	}
	cmd.AddCommand(newCapacityClearCommand(configPath, host, port, opts))
	return cmd
}

func newCapacityClearCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var projectID string
	var scope string
	cmd := &cobra.Command{
		Use:     "clear",
		Short:   "Clear recorded provider capacity outages",
		Example: "detent capacity clear\n  detent capacity clear --project detent --scope codex",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := runCapacityClear(cmd.Context(), derefString(configPath), derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), projectID, scope, opts)
			if err != nil {
				return err
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			return out.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintf(writer, "cleared %d capacity outage(s)\n", result.Cleared)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "limit the clear to one project ID")
	cmd.Flags().StringVar(&scope, "scope", "", "limit the clear to a backend ID, kind, provider, or backend/provider")
	return cmd
}

func runCapacityClear(
	ctx context.Context,
	configPath string,
	host string,
	port int,
	portSet bool,
	projectID string,
	scope string,
	opts options,
) (capacityClearResult, error) {
	boot, err := resolveDashboardBoot(ctx, configPath, host, port, portSet, opts)
	if err != nil {
		return capacityClearResult{}, err
	}
	form := url.Values{}
	form.Set("project_id", strings.TrimSpace(projectID))
	form.Set("scope", strings.TrimSpace(scope))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+dashboardServerAddr(boot)+"/api/v1/capacity/clear", strings.NewReader(form.Encode()))
	if err != nil {
		return capacityClearResult{}, fmt.Errorf("create capacity clear request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	apiToken := strings.TrimSpace(opts.lookupEnv("DETENT_API_TOKEN"))
	if apiToken == "" {
		apiToken = strings.TrimSpace(boot.Global.APIToken)
	}
	if apiToken != "" {
		request.Header.Set("Authorization", "Bearer "+apiToken)
	}
	response, err := opts.httpDo(request)
	if err != nil {
		return capacityClearResult{}, fmt.Errorf("clear capacity outage: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr != nil {
			return capacityClearResult{}, fmt.Errorf("clear capacity outage: HTTP %d", response.StatusCode)
		}
		return capacityClearResult{}, fmt.Errorf("clear capacity outage: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result capacityClearResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return capacityClearResult{}, fmt.Errorf("decode capacity clear response: %w", err)
	}
	return result, nil
}

func dashboardServerAddr(boot BootConfig) string {
	host := unbracketIPv6Host(strings.TrimSpace(boot.Host))
	switch host {
	case "", "0.0.0.0", "::":
		host = defaultWebHost
	}
	boot.Host = host
	return serverAddr(boot)
}

func resolveDashboardBoot(
	ctx context.Context,
	configPath string,
	host string,
	port int,
	portSet bool,
	opts options,
) (BootConfig, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return BootConfig{}, err
	}
	cfg, err := opts.read(resolution.Path)
	if err != nil {
		return BootConfig{}, err
	}
	portSetting, err := resolveRuntimePort(ctx, runtimeInput{
		Config:     &cfg,
		ConfigPath: resolution,
		Workflow:   firstGlobalWorkflowPath(cfg),
		Flags: runtimeFlags{
			Port: runtimeIntFlag{Value: port, Set: portSet},
		},
	}, runtimeDepsFromOptions(opts))
	if err != nil {
		return BootConfig{}, err
	}
	resolvedPort := portSetting.Value
	return BootConfig{
		Mode:           BootModeRunning,
		Global:         cfg,
		ConfigPathRule: resolution.Rule,
		Host:           bootHost(ctx, host, firstGlobalProject(cfg)),
		Port:           &resolvedPort,
	}, nil
}
