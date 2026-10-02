package profiling

import (
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*Config)
		bad  bool
	}{
		{"disabled", func(*Config) {}, false},
		{"IPv4", func(c *Config) { c.ListenAddr = "127.0.0.1:0" }, false},
		{"IPv6", func(c *Config) { c.ListenAddr = "[::1]:6060" }, false},
		{"localhost", func(c *Config) { c.ListenAddr = "localhost:6060" }, false},
		{"wildcard IPv4", func(c *Config) { c.ListenAddr = "0.0.0.0:6060" }, true},
		{"wildcard IPv6", func(c *Config) { c.ListenAddr = "[::]:6060" }, true},
		{"missing host", func(c *Config) { c.ListenAddr = ":6060" }, true},
		{"public IP", func(c *Config) { c.ListenAddr = "192.0.2.1:6060" }, true},
		{"hostname", func(c *Config) { c.ListenAddr = "example.com:6060" }, true},
		{"invalid port", func(c *Config) { c.ListenAddr = "127.0.0.1:65536" }, true},
		{"service port", func(c *Config) { c.ListenAddr = "127.0.0.1:http" }, true},
		{"negative interval", func(c *Config) { c.Capture.Interval = -time.Second }, true},
		{"negative cpu", func(c *Config) { c.Capture.CPUDuration = -time.Second }, true},
		{"overlapping cpu", func(c *Config) { c.Capture.CPUDuration = time.Hour }, true},
		{"negative age", func(c *Config) { c.Capture.MaxAge = -time.Second }, true},
		{"negative size", func(c *Config) { c.Capture.MaxBytes = -1 }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{}
			tt.edit(&cfg)
			if err := cfg.Validate(); (err != nil) != tt.bad {
				t.Fatalf("Validate() = %v, want error %v", err, tt.bad)
			}
		})
	}
}
