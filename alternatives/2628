package profiling

import (
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Config)
		valid  bool
	}{
		{"off", func(*Config) {}, true},
		{"IPv4", func(c *Config) { c.ListenAddr = "127.0.0.1:0" }, true},
		{"IPv6", func(c *Config) { c.ListenAddr = "[::1]:0" }, true},
		{"localhost", func(c *Config) { c.ListenAddr = "localhost:0" }, true},
		{"wildcard", func(c *Config) { c.ListenAddr = ":6060" }, false},
		{"IPv4 wildcard", func(c *Config) { c.ListenAddr = "0.0.0.0:6060" }, false},
		{"IPv6 wildcard", func(c *Config) { c.ListenAddr = "[::]:6060" }, false},
		{"public IP", func(c *Config) { c.ListenAddr = "192.0.2.1:6060" }, false},
		{"hostname", func(c *Config) { c.ListenAddr = "example.com:6060" }, false},
		{"missing port", func(c *Config) { c.ListenAddr = "127.0.0.1" }, false},
		{"invalid port", func(c *Config) { c.ListenAddr = "127.0.0.1:65536" }, false},
		{"named port", func(c *Config) { c.ListenAddr = "127.0.0.1:http" }, false},
		{"zero interval", func(c *Config) { c.Capture.Interval = 0 }, false},
		{"negative CPU", func(c *Config) { c.Capture.CPUDuration = -time.Second }, false},
		{"CPU exceeds interval", func(c *Config) { c.Capture.CPUDuration = time.Hour }, false},
		{"zero age", func(c *Config) { c.Capture.MaxAge = 0 }, false},
		{"zero size", func(c *Config) { c.Capture.MaxBytes = 0 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Default()
			test.change(&config)
			if err := config.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate() = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	for _, test := range []struct {
		input string
		want  ByteSize
	}{
		{"1GB", 1_000_000_000}, {"2GiB", 2 << 30}, {"10 MB", 10_000_000}, {"3KiB", 3 << 10}, {"123", 123},
		{"0", 0}, {"-1GB", 0}, {"1TB", 0}, {"9223372036854775807GB", 0}, {"1.5GB", 0}, {"", 0},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseBytes(test.input)
			if (err == nil) != (test.want != 0) || got != test.want {
				t.Fatalf("ParseBytes(%q) = %d, %v; want %d", test.input, got, err, test.want)
			}
		})
	}
}
