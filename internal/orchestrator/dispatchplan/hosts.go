package dispatchplan

import (
	"slices"
	"strings"
)

type HostConfig struct {
	WorkerHosts                []string
	WorkerHostSelection        string
	WorkerHostCaps             map[string]int
	MaxConcurrentAgentsPerHost int
}

type HostSelector struct {
	cfg       HostConfig
	available func(string) bool
}

func NewHostSelector(cfg HostConfig, available func(string) bool) HostSelector {
	return HostSelector{cfg: cfg, available: available}
}

func (p HostSelector) Select(count func(string) int, preferredWorkerHost string) (string, bool) {
	if len(p.cfg.WorkerHosts) == 0 {
		return "", true
	}

	availableHosts := make([]string, 0, len(p.cfg.WorkerHosts))
	for _, host := range p.cfg.WorkerHosts {
		if p.cfg.SlotsAvailable(host, count) && (p.available == nil || p.available(host)) {
			availableHosts = append(availableHosts, host)
		}
	}
	if len(availableHosts) == 0 {
		return "", false
	}

	preferredWorkerHost = strings.TrimSpace(preferredWorkerHost)
	if p.cfg.WorkerHostSelection == "preference" {
		return availableHosts[0], true
	}
	if preferredWorkerHost != "" {
		if slices.Contains(availableHosts, preferredWorkerHost) {
			return preferredWorkerHost, true
		}
	}

	return LeastLoadedHost(availableHosts, count), true
}

func (c HostConfig) SlotsAvailable(workerHost string, count func(string) int) bool {
	capacity := c.Capacity(workerHost)
	if capacity <= 0 {
		return true
	}

	return count(workerHost) < capacity
}

func (c HostConfig) Capacity(host string) int {
	if capacity := c.WorkerHostCaps[host]; capacity > 0 {
		return capacity
	}
	return c.MaxConcurrentAgentsPerHost
}

func LeastLoadedHost(hosts []string, count func(string) int) string {
	selected := hosts[0]
	selectedCount := count(selected)
	for _, host := range hosts[1:] {
		used := count(host)
		if used < selectedCount {
			selected = host
			selectedCount = used
		}
	}
	return selected
}
