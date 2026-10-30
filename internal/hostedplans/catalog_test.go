package hostedplans

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestHostedPlansConfigDecodesStrictly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body string
		wantErr    bool
		written    bool
	}{
		{name: "valid catalog", body: "base: {id: free, version: 1}\nplans:\n  - {id: free, version: 1, allowances: {projects: 3}}\n", written: true},
		{name: "empty section", body: "{}\n"},
		{name: "alias to an anchor outside the section", body: "base: *free\nplans:\n  - {id: free, version: 1}\n", written: true},
		{name: "merge key from an anchor outside the section", body: "base: {<<: *free}\n", written: true},
		{name: "misspelled nested key", body: "base: {id: free, version: 1}\nplans:\n  - {id: free, version: 1, allowences: {projects: 3}}\n", wantErr: true},
		{name: "unknown plan reference key", body: "base: {id: free, release: 1}\n", wantErr: true},
		{name: "misspelled section key", body: "windows_seconds: 3600\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			decoder := yaml.NewDecoder(strings.NewReader("free: &free {id: free, version: 1}\nentitlements:\n" + indentYAML(test.body)))
			decoder.KnownFields(true)
			var document struct {
				Free         PlanReference     `yaml:"free"`
				Entitlements HostedPlansConfig `yaml:"entitlements"`
			}
			err := decoder.Decode(&document)
			config := document.Entitlements
			if test.wantErr {
				if err == nil {
					t.Fatalf("Decode() = %+v, want an unknown-field error", config)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.written != test.written {
				t.Fatalf("written = %t, want %t", config.written, test.written)
			}
		})
	}
}

func indentYAML(body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestHostedPlansConfigRejectsAliasCycles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, document string
		wantErr        bool
	}{
		{name: "self-referential section", document: "entitlements: &e {base: *e}\n", wantErr: true},
		{name: "cycle through a plan", document: "entitlements:\n  plans:\n    - &p {id: free, version: 1, features: [*p]}\n", wantErr: true},
		{name: "one anchor used twice", document: "free: &free {id: free, version: 1}\nentitlements:\n  base: *free\n  plans:\n    - {<<: *free}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var document struct {
				Free         PlanReference     `yaml:"free"`
				Entitlements HostedPlansConfig `yaml:"entitlements"`
			}
			done := make(chan error, 1)
			go func() { done <- yaml.Unmarshal([]byte(test.document), &document) }()
			select {
			case err := <-done:
				if (err != nil) != test.wantErr {
					t.Fatalf("Unmarshal() error = %v, want error %t", err, test.wantErr)
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("decoding did not finish")
			}
		})
	}
}

func TestPlanConfigurationValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*HostedPlansConfig)
	}{
		{"missing base", func(c *HostedPlansConfig) { c.Base.ID = "absent" }},
		{"negative allowance", func(c *HostedPlansConfig) { c.Plans[0].Allowances["projects"] = -1 }},
		{"unknown allowance", func(c *HostedPlansConfig) { c.Plans[0].Allowances["unlimited"] = 1 }},
		{"unbounded retention", func(c *HostedPlansConfig) { c.RetentionWindows = 721 }},
		{"unknown feature", func(c *HostedPlansConfig) { c.Plans[0].Features = []string{"admin_bypass"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Pilot()
			test.change(&c)
			if c.Validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
