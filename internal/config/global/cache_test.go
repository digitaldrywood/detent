package global

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestParseGlobalCache(t *testing.T) {
	for _, tt := range []struct {
		name, yaml string
		age        time.Duration
		bytes      int64
		bad        bool
	}{
		{"defaults", "", 48 * time.Hour, (toolcache.Policy{}).Normalized().MaxBytes, false},
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
