package mutation

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBindRejectsInvalidIdentity(t *testing.T) {
	t.Parallel()
	base := Metadata{PrincipalID: "actor", OrganizationID: "org", ProjectID: "project", Action: "file_issue"}
	for _, test := range []struct {
		name string
		edit func(*Metadata)
		key  string
	}{
		{"missing principal", func(m *Metadata) { m.PrincipalID = "" }, "key"},
		{"missing organization", func(m *Metadata) { m.OrganizationID = "" }, "key"},
		{"missing operation", func(m *Metadata) { m.Action = "" }, "key"},
		{"missing key", func(*Metadata) {}, ""},
		{"oversized key", func(*Metadata) {}, strings.Repeat("k", 129)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := base
			test.edit(&input)
			got, err := input.Bind(test.key, "input")
			if err == nil || got != input {
				t.Fatalf("Bind() = %+v, %v; want unchanged rejected identity", got, err)
			}
		})
	}
}

func TestBindProjectIsolationAndEncoding(t *testing.T) {
	t.Parallel()
	base := Metadata{PrincipalID: "actor", OrganizationID: "org", ProjectID: "first", Action: "file_issue"}
	key := strings.Repeat("k", 128)
	first, err := base.Bind(key, map[string]int{"a": 1, "b": 2})
	if err != nil {
		t.Fatal(err)
	}
	base.ProjectID = "second"
	second, err := base.Bind(key, map[string]int{"b": 2, "a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.RetryIdentity == second.RetryIdentity || first.InputHash != second.InputHash {
		t.Fatalf("project identities or canonical input hashes differ incorrectly: %+v, %+v", first, second)
	}
	if _, err := base.Bind(key, make(chan int)); err == nil {
		t.Fatal("Bind() accepted input that cannot be encoded")
	}
}

func TestErrorTextContextIsolation(t *testing.T) {
	t.Parallel()
	err := errors.New("provider diagnostic with sensitive payload")
	for _, test := range []struct {
		name, source, want string
	}{
		{"mcp redacts", "mcp", "application mutation unavailable"},
		{"dashboard diagnostic", "dashboard", err.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			parent := context.Background()
			metadata := Metadata{PrincipalID: "actor", Source: test.source}
			ctx := WithContext(parent, metadata)
			if got, ok := FromContext(ctx); !ok || got != metadata {
				t.Fatalf("FromContext() = %+v, %t", got, ok)
			}
			if _, ok := FromContext(parent); ok || ErrorText(parent, err) != err.Error() {
				t.Fatal("child mutation context leaked into its parent")
			}
			if got := ErrorText(ctx, err); got != test.want {
				t.Fatalf("ErrorText() = %q, want %q", got, test.want)
			}
		})
	}
}
