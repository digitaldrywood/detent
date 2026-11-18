package cli

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/serviceapi"
)

type dashboardAddress struct {
	Value      string
	HostSource string
	PortSource string
}

const (
	dashboardAddressSourceServiceFlag       = "service flag"
	dashboardAddressSourceListener          = "running listener"
	dashboardAddressSourceWorkerEnvironment = "worker environment"
)

func (a dashboardAddress) String() string {
	if a.Value == "" {
		return ""
	}
	if a.HostSource == a.PortSource {
		return fmt.Sprintf("%s (from %s)", a.Value, a.HostSource)
	}
	return fmt.Sprintf("%s (host from %s, port from %s)", a.Value, a.HostSource, a.PortSource)
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
	cfg, err = resolveRunnerProjectsReadOnly(ctx, cfg)
	if err != nil {
		return BootConfig{}, dashboardAddress{}, err
	}
	return resolveDashboardBootConfig(ctx, resolution, cfg, host, port, portSet, opts)
}

func resolveDashboardBootConfig(
	ctx context.Context,
	resolution globalconfig.PathResolution,
	cfg globalconfig.Config,
	host string,
	port int,
	portSet bool,
	opts options,
) (BootConfig, dashboardAddress, error) {
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
	if dashboardAddressMayUseWorkerEnvironment(hostSetting.Source) || dashboardAddressMayUseWorkerEnvironment(portSetting.Source) {
		workerHost, workerPort, workerAddressSet, workerAddressErr := workerServiceAddress(opts.lookupEnv)
		if workerAddressErr != nil {
			return BootConfig{}, dashboardAddress{}, workerAddressErr
		}
		if workerAddressSet {
			if dashboardAddressMayUseWorkerEnvironment(hostSetting.Source) {
				hostSetting = RuntimeValue{Value: workerHost, Source: dashboardAddressSourceWorkerEnvironment}
			}
			if dashboardAddressMayUseWorkerEnvironment(portSetting.Source) {
				portSetting = RuntimeIntValue{Value: workerPort, Source: dashboardAddressSourceWorkerEnvironment}
			}
		}
	}
	if inspection, err := instancelock.Inspect(filepath.Join(filepath.Dir(resolution.Path), "detent.db.lock")); err == nil && inspection.Status == instancelock.StatusHeld && inspection.MetadataError == nil {
		listenerHost, listenerPort, splitErr := net.SplitHostPort(inspection.Owner.DashboardAddress)
		if p, valid := validServicePort(listenerPort); splitErr == nil && listenerHost != "" && valid && p > 0 && p <= 65535 {
			if dashboardAddressMayUseServiceFlag(hostSetting.Source) {
				hostSetting = RuntimeValue{Value: listenerHost, Source: dashboardAddressSourceListener}
			}
			if dashboardAddressMayUseServiceFlag(portSetting.Source) {
				portSetting = RuntimeIntValue{Value: p, Source: dashboardAddressSourceListener}
			}
		}
	}
	if dashboardAddressMayUseServiceFlag(hostSetting.Source) || dashboardAddressMayUseServiceFlag(portSetting.Source) {
		serviceArguments := runningServiceArguments(ctx, resolution.Path, opts)
		if dashboardAddressMayUseServiceFlag(hostSetting.Source) {
			if serviceHost, ok := serviceStringFlag(serviceArguments, "--host"); ok {
				hostSetting = RuntimeValue{Value: serviceHost, Source: dashboardAddressSourceServiceFlag}
			}
		}
		if dashboardAddressMayUseServiceFlag(portSetting.Source) {
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

func dashboardAddressMayUseWorkerEnvironment(source string) bool {
	return source != runtimeSourceFlag
}

func dashboardAddressMayUseServiceFlag(source string) bool {
	return source != runtimeSourceFlag && source != dashboardAddressSourceWorkerEnvironment && source != dashboardAddressSourceListener
}

func workerServiceAddress(lookupEnv func(string) string) (string, int, bool, error) {
	if lookupEnv == nil {
		return "", 0, false, nil
	}
	raw := strings.TrimSpace(lookupEnv(serviceapi.AddressEnvironment))
	if raw == "" {
		return "", 0, false, nil
	}
	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, false, fmt.Errorf("invalid %s %q: %w", serviceapi.AddressEnvironment, raw, err)
	}
	port, ok := validServicePort(portText)
	if strings.TrimSpace(host) == "" || !ok || port == 0 || port > 65535 {
		return "", 0, false, fmt.Errorf("invalid %s %q: want host:port with a nonzero TCP port", serviceapi.AddressEnvironment, raw)
	}
	return host, port, true, nil
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
