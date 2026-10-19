package global

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseGlobalCache(t *testing.T) {
	for _, tt := range []struct {
		name, yaml string
		age        time.Duration
		bytes      int64
		bad        bool
	}{
		{"defaults", "", 48 * time.Hour, 0, false},
		{"explicit", "  cache: {max_age: 12h, max_bytes: 123}\n", 12 * time.Hour, 123, false},
		{"negative age", "  cache: {max_age: -1h}\n", 0, 0, true},
		{"negative size", "  cache: {max_bytes: -1}\n", 0, 0, true},
		{"invalid age", "  cache: {max_age: yesterday}\n", 0, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := "apiVersion: detent/v1\nkind: GlobalConfig\nglobal:\n  max_concurrent_agents: 2\n  scheduling: weighted\n" + tt.yaml + "projects: []\n"
			cfg, err := Parse([]byte(raw), "", WithHome(t.TempDir()))
			if (err != nil) != tt.bad {
				t.Fatalf("Parse error=%v", err)
			}
			if tt.bad {
				return
			}
			if cfg.Global.Cache.MaxAge != tt.age || cfg.Global.Cache.MaxBytes != tt.bytes {
				t.Fatalf("cache=%+v", cfg.Global.Cache)
			}
		})
	}
}

func TestWritePreservesCacheDefault(t *testing.T) {
	for _, tt := range []struct {
		name, cache string
		want        int64
	}{
		{"omitted", "", 0}, {"age only", "  cache: {max_age: 12h}\n", 0}, {"explicit", "  cache: {max_bytes: 123}\n", 123},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "global.yaml")
			raw := "apiVersion: detent/v1\nkind: GlobalConfig\nglobal:\n  max_concurrent_agents: 2\n  scheduling: weighted\n" + tt.cache + "projects: []\n"
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cfg, err := Read(path, WithHome(root))
				if err != nil {
					t.Fatal(err)
				}
				cfg.Global.MaxConcurrentAgents++
				if err := Write(path, cfg, WithHome(root)); err != nil {
					t.Fatal(err)
				}
				got, err := Read(path, WithHome(root))
				if err != nil {
					t.Fatal(err)
				}
				if got.Global.Cache.MaxBytes != tt.want {
					t.Fatalf("bound=%d want %d", got.Global.Cache.MaxBytes, tt.want)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if tt.want == 0 && strings.Contains(string(data), "max_bytes:") {
					t.Fatalf("default serialized: %s", data)
				}
			}
		})
	}
}
