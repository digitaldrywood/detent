package web

import (
	"os"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

const maxInstanceNameRunes = 40

type resolvedInstanceName struct {
	Name      string
	Truncated bool
}

func resolveInstanceName(cfg globalconfig.Config, hostname string) resolvedInstanceName {
	for _, candidate := range []string{
		cfg.InstanceName,
		cfg.Global.Identity.Name,
		shortHostname(hostname),
	} {
		resolved := normalizeInstanceName(candidate)
		if resolved.Name != "" {
			return resolved
		}
	}
	return resolvedInstanceName{}
}

func normalizeInstanceName(value string) resolvedInstanceName {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return resolvedInstanceName{}
	}

	runes := []rune(value)
	if len(runes) <= maxInstanceNameRunes {
		return resolvedInstanceName{Name: value}
	}
	return resolvedInstanceName{
		Name:      string(runes[:maxInstanceNameRunes-1]) + "…",
		Truncated: true,
	}
}

func shortHostname(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if before, _, ok := strings.Cut(hostname, "."); ok {
		return before
	}
	return hostname
}

func instancePageTitle(instanceName string, base string) string {
	base = strings.TrimSpace(base)
	instanceName = strings.TrimSpace(instanceName)
	if instanceName == "" {
		return base
	}
	return instanceName + " · " + base
}

func applicationName(instanceName string) string {
	return instancePageTitle(instanceName, "Detent")
}

func (cfg Config) globalConfigSource() func() globalconfig.Config {
	if cfg.GlobalConfigSource != nil {
		return cfg.GlobalConfigSource
	}
	return func() globalconfig.Config {
		return cfg.GlobalConfig
	}
}

func (cfg Config) hostname() func() (string, error) {
	if cfg.Hostname != nil {
		return cfg.Hostname
	}
	return os.Hostname
}

func (s *Server) currentGlobalConfig() globalconfig.Config {
	return s.globalConfigSource()
}

func (s *Server) instanceName() string {
	resolved := s.resolvedInstanceName()
	return resolved.Name
}

func (s *Server) resolvedInstanceName() resolvedInstanceName {
	hostname := ""
	if s.hostname != nil {
		value, err := s.hostname()
		if err != nil {
			s.logger.Warn("resolve hostname failed", "error", err)
		} else {
			hostname = value
		}
	}

	resolved := resolveInstanceName(s.currentGlobalConfig(), hostname)
	if resolved.Truncated {
		s.logger.Warn("instance name truncated", "max_characters", maxInstanceNameRunes)
	}
	return resolved
}
