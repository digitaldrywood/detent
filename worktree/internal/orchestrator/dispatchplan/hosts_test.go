package dispatchplan

import "testing"

func TestSSHHostPreferenceAndCaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, selection, preferred string
		used                       map[string]int
		down                       string
		want                       string
		ok                         bool
	}{
		{"fill first", "preference", "", map[string]int{"air": 1}, "", "air", true},
		{"spill second", "preference", "", map[string]int{"air": 2, "studio": 1}, "", "studio", true},
		{"spill local", "preference", "", map[string]int{"air": 2, "studio": 3}, "", "local", true},
		{"all full", "preference", "", map[string]int{"air": 2, "studio": 3, "local": 1}, "", "", false},
		{"least loaded default", "", "", map[string]int{"air": 1}, "", "studio", true},
		{"least loaded retry affinity", "least_loaded", "air", map[string]int{"air": 1}, "", "air", true},
		{"preference beats retry affinity", "preference", "studio", nil, "", "air", true},
		{"skip unreachable", "preference", "air", nil, "air", "studio", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := HostConfig{WorkerHosts: []string{"air", "studio", "local"}, WorkerHostSelection: test.selection, WorkerHostCaps: map[string]int{"air": 2, "studio": 3}, MaxConcurrentAgentsPerHost: 1}
			planner := NewHostSelector(cfg, func(host string) bool { return host != test.down })
			got, ok := planner.Select(func(host string) int { return test.used[host] }, test.preferred)
			if got != test.want || ok != test.ok {
				t.Fatalf("selection = %q, %v; want %q, %v", got, ok, test.want, test.ok)
			}
		})
	}
}
