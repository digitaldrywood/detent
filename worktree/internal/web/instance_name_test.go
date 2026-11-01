package web

import (
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestResolveInstanceName(t *testing.T) {
	t.Parallel()

	longName := "abcdefghijklmnopqrstuvwxyz0123456789ABCDE"
	tests := []struct {
		name          string
		config        globalconfig.Config
		hostname      string
		want          string
		wantTruncated bool
	}{
		{
			name: "global instance name wins",
			config: globalconfig.Config{
				InstanceName: " buildbox ",
				Global:       globalconfig.Settings{Identity: globalconfig.Identity{Name: "identity-name"}},
			},
			hostname: "host.example.com",
			want:     "buildbox",
		},
		{
			name: "global identity fallback",
			config: globalconfig.Config{
				Global: globalconfig.Settings{Identity: globalconfig.Identity{Name: "identity-name"}},
			},
			hostname: "host.example.com",
			want:     "identity-name",
		},
		{
			name:     "hostname fallback uses short host",
			hostname: "runner-01.example.com",
			want:     "runner-01",
		},
		{
			name: "blank values fall through",
			config: globalconfig.Config{
				InstanceName: " ",
				Global:       globalconfig.Settings{Identity: globalconfig.Identity{Name: " "}},
			},
			hostname: " ",
			want:     "",
		},
		{
			name: "long name truncates to forty characters",
			config: globalconfig.Config{
				InstanceName: longName,
			},
			hostname:      "host.example.com",
			want:          string([]rune(longName)[:39]) + "…",
			wantTruncated: true,
		},
		{
			name: "newline candidate is rejected",
			config: globalconfig.Config{
				InstanceName: "bad\nname",
				Global:       globalconfig.Settings{Identity: globalconfig.Identity{Name: "identity-name"}},
			},
			hostname: "host.example.com",
			want:     "identity-name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveInstanceName(tt.config, tt.hostname)
			if got.Name != tt.want {
				t.Fatalf("Name = %q, want %q", got.Name, tt.want)
			}
			if got.Truncated != tt.wantTruncated {
				t.Fatalf("Truncated = %v, want %v", got.Truncated, tt.wantTruncated)
			}
			if len([]rune(got.Name)) > maxInstanceNameRunes {
				t.Fatalf("Name length = %d, want at most %d", len([]rune(got.Name)), maxInstanceNameRunes)
			}
		})
	}
}

func TestInstancePageTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		instance string
		base     string
		want     string
	}{
		{name: "with instance", instance: "buildbox", base: "Detent reports", want: "buildbox · Detent reports"},
		{name: "without instance", instance: "", base: "Detent reports", want: "Detent reports"},
		{name: "trim values", instance: " buildbox ", base: " Detent ", want: "buildbox · Detent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := instancePageTitle(tt.instance, tt.base); got != tt.want {
				t.Fatalf("instancePageTitle() = %q, want %q", got, tt.want)
			}
			if strings.Contains(instancePageTitle("", tt.base), " · ") {
				t.Fatalf("instancePageTitle without instance contains separator")
			}
		})
	}
}
