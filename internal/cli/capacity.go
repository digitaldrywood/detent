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

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

type capacityClearResult struct {
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Cleared int    `json:"cleared"`
}

type dashboardAddress struct {
	Value      string
	HostSource string
	PortSource string
}

const dashboardAddressSourceServiceFlag = "service flag"

func (a dashboardAddress) String() string {
	if a.Value == "" {
		return ""
	}
	if a.HostSource == a.PortSource {
		return fmt.Sprintf("%s (from %s)", a.Value, a.HostSource)
	}
	return fmt.Sprintf("%s (host from %s, port from %s)", a.Value, a.HostSource, a.PortSource)
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
	boot, address, err := resolveDashboardBoot(ctx, configPath, host, port, portSet, opts)
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
		return capacityClearResult{}, fmt.Errorf("clear capacity outage via %s: %w", address, err)
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
) (BootConfig, dashboardAddress, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return BootConfig{}, dashboardAddress{}, err
	}
	cfg, err := opts.read(resolution.Path)
	if err != nil {
		return BootConfig{}, dashboardAddress{}, err
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
		return BootConfig{}, dashboardAddress{}, err
	}
	hostSetting := resolveDashboardHost(ctx, host, firstGlobalProject(cfg))
	if hostSetting.Source != runtimeSourceFlag || portSetting.Source != runtimeSourceFlag {
		serviceArguments := runningServiceArguments(ctx, resolution.Path, opts)
		if hostSetting.Source != runtimeSourceFlag {
			if serviceHost, ok := serviceStringFlag(serviceArguments, "--host"); ok {
				hostSetting = RuntimeValue{Value: serviceHost, Source: dashboardAddressSourceServiceFlag}
			}
		}
		if portSetting.Source != runtimeSourceFlag {
			if servicePort, ok := serviceIntFlag(serviceArguments, "--port"); ok {
				portSetting = RuntimeIntValue{Value: servicePort, Source: dashboardAddressSourceServiceFlag}
			}
		}
	}
	resolvedPort := portSetting.Value
	boot := BootConfig{
		Mode:           BootModeRunning,
		Global:         cfg,
		ConfigPathRule: resolution.Rule,
		Host:           hostSetting.Value,
		Port:           &resolvedPort,
	}
	return boot, dashboardAddress{
		Value:      dashboardServerAddr(boot),
		HostSource: dashboardAddressSource(hostSetting.Source),
		PortSource: dashboardAddressSource(portSetting.Source),
	}, nil
}

func resolveDashboardHost(ctx context.Context, host string, project globalconfig.Project) RuntimeValue {
	if host = strings.TrimSpace(host); host != "" {
		return RuntimeValue{Value: host, Source: runtimeSourceFlag}
	}
	if host = bootHost(ctx, "", project); host != "" {
		return RuntimeValue{Value: host, Source: runtimeSourceConfig}
	}
	return RuntimeValue{Value: defaultWebHost, Source: runtimeSourceDefault}
}

func dashboardAddressSource(source string) string {
	if source == runtimeSourceWorkflow {
		return runtimeSourceConfig
	}
	return sourceDetail(source)
}

func serviceStringFlag(arguments []string, name string) (string, bool) {
	var value string
	var found bool
	for index, argument := range arguments {
		if argument == name && index+1 < len(arguments) {
			value = strings.TrimSpace(arguments[index+1])
			found = value != ""
		}
		if raw, ok := strings.CutPrefix(argument, name+"="); ok {
			value = strings.TrimSpace(raw)
			found = value != ""
		}
	}
	return value, found
}

func serviceIntFlag(arguments []string, name string) (int, bool) {
	raw, ok := serviceStringFlag(arguments, name)
	if !ok {
		return 0, false
	}
	return validServicePort(raw)
}
