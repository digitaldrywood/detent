package profiling

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string  `yaml:"listen_addr,omitempty"`
	Capture    Capture `yaml:"capture,omitempty"`
}

type Capture struct {
	Enabled     bool          `yaml:"enabled"`
	Interval    time.Duration `yaml:"interval"`
	CPUDuration time.Duration `yaml:"cpu_duration"`
	Dir         string        `yaml:"dir,omitempty"`
	MaxAge      time.Duration `yaml:"max_age"`
	MaxBytes    Bytes         `yaml:"max_bytes"`
}

type Bytes int64

func (c Config) IsZero() bool {
	return c.Normalized() == (Config{}).Normalized()
}

func (c Config) MarshalYAML() (any, error) {
	type plain Config
	return plain(c.Normalized()), nil
}

func (b *Bytes) UnmarshalYAML(decode func(any) error) error {
	var value string
	if err := decode(&value); err != nil {
		return err
	}
	value = strings.ToUpper(strings.TrimSpace(value))
	multiplier := int64(1)
	for _, unit := range []struct {
		name string
		size int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}, {"B", 1}} {
		if strings.HasSuffix(value, unit.name) {
			value = strings.TrimSpace(strings.TrimSuffix(value, unit.name))
			multiplier = unit.size
			break
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || n > math.MaxInt64/multiplier {
		return errors.New("profiling.capture.max_bytes must be a positive byte count or size such as 1GB")
	}
	*b = Bytes(n * multiplier)
	return nil
}

func DefaultCapture() Capture {
	return Capture{Interval: 15 * time.Minute, CPUDuration: 30 * time.Second, MaxAge: 168 * time.Hour, MaxBytes: 1_000_000_000}
}

func (c *Capture) UnmarshalYAML(decode func(any) error) error {
	type plain Capture
	value := plain(DefaultCapture())
	if err := decode(&value); err != nil {
		return err
	}
	*c = Capture(value)
	return c.validate()
}

func (c Config) Normalized() Config {
	defaults := DefaultCapture()
	if c.Capture.Interval == 0 {
		c.Capture.Interval = defaults.Interval
	}
	if c.Capture.CPUDuration == 0 {
		c.Capture.CPUDuration = defaults.CPUDuration
	}
	if c.Capture.MaxAge == 0 {
		c.Capture.MaxAge = defaults.MaxAge
	}
	if c.Capture.MaxBytes == 0 {
		c.Capture.MaxBytes = defaults.MaxBytes
	}
	return c
}

func (c Config) Validate() error {
	if c.ListenAddr != "" {
		if _, err := loopbackAddress(c.ListenAddr); err != nil {
			return err
		}
	}
	return c.Normalized().Capture.validate()
}

func (c Capture) validate() error {
	if c.Interval <= 0 || c.CPUDuration <= 0 || c.CPUDuration > c.Interval {
		return errors.New("profiling.capture requires positive interval and cpu_duration, with cpu_duration <= interval")
	}
	if c.MaxAge <= 0 || c.MaxBytes <= 0 {
		return errors.New("profiling.capture.max_age and max_bytes must be positive")
	}
	return nil
}

func loopbackAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("profiling.listen_addr must be a loopback host:port: %w", err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	p, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || p < 0 || p > 65535 {
		return "", errors.New("profiling.listen_addr must use a loopback IP (or localhost) and a numeric port")
	}
	return net.JoinHostPort(ip.String(), port), nil
}
