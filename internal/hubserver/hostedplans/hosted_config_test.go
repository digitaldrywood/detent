package hostedplans

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
)

type sharedConfigProvider struct {
	auth.HostedProvider
}

func TestHostedSharedConfigValidation(t *testing.T) {
	t.Parallel()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	valid := func() *HostedConfig {
		return &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "https://hub.example.test", Provider: sharedConfigProvider{},
			SharedEntry: &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key}, Generation: 1}}
	}
	tests := []struct {
		name   string
		mutate func(*HostedConfig)
		ok     bool
	}{
		{"valid", func(*HostedConfig) {}, true},
		{"missing issuer", func(c *HostedConfig) { c.SharedEntry.Issuer = "" }, false},
		{"zero generation", func(c *HostedConfig) { c.SharedEntry.Generation = 0 }, false},
		{"no keys", func(c *HostedConfig) { c.SharedEntry.PublicKeys = nil }, false},
		{"short key", func(c *HostedConfig) { c.SharedEntry.PublicKeys = []ed25519.PublicKey{key[:5]} }, false},
		{"directory", func(c *HostedConfig) {
			c.Directory = []HostedDestination{{OrganizationID: "org_b", WorkOSOrganizationID: "org_provider_b", PublicURL: "https://b.example.test"}}
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid()
			test.mutate(cfg)
			if err := ValidateHostedConfig(cfg); (err == nil) != test.ok {
				t.Fatalf("validate error = %v, want ok %v", err, test.ok)
			}
		})
	}
}
