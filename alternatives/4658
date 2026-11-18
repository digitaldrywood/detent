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
	ListenAddr string        `yaml:"listen_addr,omitempty"`
	Capture    CaptureConfig `yaml:"capture,omitempty"`
}

type CaptureConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Interval    time.Duration `yaml:"interval"`
	CPUDuration time.Duration `yaml:"cpu_duration"`
	Dir         string        `yaml:"dir,omitempty"`
	MaxAge      time.Duration `yaml:"max_age"`
	MaxBytes    ByteSize      `yaml:"max_bytes"`
}

type ByteSize int64

func Default() Config {
	return Config{Capture: CaptureConfig{Interval: 15 * time.Minute, CPUDuration: 30 * time.Second, MaxAge: 168 * time.Hour, MaxBytes: 1_000_000_000}}
}

func (c Config) IsZero() bool {
	return c == (Config{}) || c == Default()
}

func (c Config) Validate() error {
	var problems []error
	if c.ListenAddr != "" {
		if _, err := loopbackAddress(c.ListenAddr); err != nil {
			problems = append(problems, fmt.Errorf("profiling.listen_addr: %w", err))
		}
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"interval", c.Capture.Interval}, {"cpu_duration", c.Capture.CPUDuration}, {"max_age", c.Capture.MaxAge},
	} {
		if field.value <= 0 {
			problems = append(problems, fmt.Errorf("profiling.capture.%s: must be positive", field.name))
		}
	}
	if c.Capture.CPUDuration > c.Capture.Interval {
		problems = append(problems, errors.New("profiling.capture.cpu_duration: must not exceed interval"))
	}
	if c.Capture.MaxBytes <= 0 {
		problems = append(problems, errors.New("profiling.capture.max_bytes: must be positive"))
	}
	return errors.Join(problems...)
}

func (c *Config) UnmarshalYAML(unmarshal func(any) error) error {
	type plain Config
	decoded := plain(Default())
	if err := unmarshal(&decoded); err != nil {
		return fmt.Errorf("profiling: %w", err)
	}
	*c = Config(decoded)
	return c.Validate()
}

func loopbackAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("must be a loopback host:port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return "", errors.New("port must be an integer from 0 to 65535")
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("host must be localhost or a loopback IP address")
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func (b *ByteSize) UnmarshalYAML(unmarshal func(any) error) error {
	var value any
	if err := unmarshal(&value); err != nil {
		return err
	}
	var size int64
	switch value := value.(type) {
	case int:
		size = int64(value)
	case int64:
		size = value
	case string:
		parsed, err := ParseBytes(value)
		if err != nil {
			return err
		}
		size = int64(parsed)
	default:
		return errors.New("max_bytes must be an integer byte count or size such as 1GB")
	}
	if size <= 0 {
		return errors.New("max_bytes must be positive")
	}
	*b = ByteSize(size)
	return nil
}

func ParseBytes(value string) (ByteSize, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	multiplier := int64(1)
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}, {"B", 1},
	} {
		if strings.HasSuffix(value, unit.suffix) {
			value = strings.TrimSpace(strings.TrimSuffix(value, unit.suffix))
			multiplier = unit.bytes
			break
		}
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size <= 0 || size > math.MaxInt64/multiplier {
		return 0, errors.New("max_bytes must be a positive integer size within int64 bytes")
	}
	return ByteSize(size * multiplier), nil
}
