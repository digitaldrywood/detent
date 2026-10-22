package cli

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

func secretStatusEnvironment(active string, versions ...string) func(string) string {
	encoded := map[string]string{}
	for _, version := range versions {
		encoded[version] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte(version), 32))
	}
	raw, _ := json.Marshal(encoded)
	return func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			if active == "" && len(versions) == 0 {
				return ""
			}
			return string(raw)
		}
		return active
	}
}

func secretStatusFixture(t *testing.T, path string, oldVersion, empty bool) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, query := range []string{
		`CREATE TABLE project_secrets (organization_id TEXT, project_id TEXT, kind TEXT, organization_slug TEXT, ciphertext BLOB, nonce BLOB, wrapped_data_key BLOB, master_key_version INTEGER, updated_at TEXT, PRIMARY KEY (organization_id, project_id, kind))`,
		`CREATE TABLE organization_secrets (organization_id TEXT, kind TEXT, ciphertext BLOB, nonce BLOB, wrapped_data_key BLOB, master_key_version INTEGER, updated_at TEXT, PRIMARY KEY (organization_id, kind))`,
		`PRAGMA user_version=73`,
	} {
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	forbidden := []string{"example-sprites-slug", "known-plaintext-token"}
	if empty {
		return forbidden
	}
	for _, row := range []struct{ org, project, kind, version string }{
		{"org_a", "project_a", "fly_sprites_token", "1"},
		{"org_b", "project_b", "fly_sprites_token", "2"},
		{"org_a", "", "slack_webhook", "1"},
		{"org_b", "", "fly_sprites_token", "2"},
	} {
		version := row.version
		if !oldVersion {
			version = "2"
		}
		keys, err := hubsecrets.FromEnvironment(secretStatusEnvironment(version, "1", "2"))
		if err != nil {
			t.Fatal(err)
		}
		aad, err := json.Marshal([3]string{row.org, row.project, row.kind})
		if err != nil {
			t.Fatal(err)
		}
		envelope, err := keys.Seal([]byte("known-plaintext-token"), aad)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range [][]byte{envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey} {
			forbidden = append(forbidden, base64.StdEncoding.EncodeToString(value))
		}
		query := `INSERT INTO organization_secrets VALUES (?, ?, ?, ?, ?, ?, ?)`
		args := []any{row.org, row.kind, envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, "2026-10-01T00:00:00Z"}
		if row.project != "" {
			query = `INSERT INTO project_secrets VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
			args = []any{row.org, row.project, row.kind, "example-sprites-slug", envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, "2026-10-01T00:00:00Z"}
		}
		if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	return forbidden
}

func runSecretStatus(t *testing.T, lookup func(string) string, wantExit int, args ...string) (hubSecretsStatus, string) {
	t.Helper()
	var output bytes.Buffer
	cmd := newHubSecretsCommand(lookup)
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"status"}, args...))
	err := cmd.ExecuteContext(t.Context())
	if got := ExitCode(err); got != wantExit {
		t.Fatalf("exit=%d want=%d error=%v output=%s", got, wantExit, err, output.String())
	}
	var report hubSecretsStatus
	decoder := json.NewDecoder(&output)
	raw := output.String()
	if err := decoder.Decode(&report); err != nil {
		t.Fatal(err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("expected one JSON document: %v", err)
	}
	return report, raw
}

func TestHubSecretsStatus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name               string
		lookup             func(string) string
		old, empty         bool
		exit               int
		problem            string
		retirable, missing []int
	}{
		{name: "incomplete", lookup: secretStatusEnvironment("2", "1", "2"), old: true, exit: 3, problem: "not_active_version"},
		{name: "missing key", lookup: secretStatusEnvironment("2", "2"), old: true, exit: 2, problem: "missing_key", missing: []int{1}},
		{name: "rotated", lookup: secretStatusEnvironment("2", "1", "2"), retirable: []int{1}},
		{name: "wrong key at same version", lookup: secretStatusEnvironment("2", "2", "3"), exit: 2, problem: "unwrap_failed"},
		{name: "disabled with rows", lookup: secretStatusEnvironment(""), old: true, exit: 2, problem: "missing_key", missing: []int{1, 2}},
		{name: "disabled without rows", lookup: secretStatusEnvironment(""), empty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "hub #?.db")
			forbidden := secretStatusFixture(t, path, test.old, test.empty)
			lookup := test.lookup
			if test.name == "wrong key at same version" {
				key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 32))
				lookup = func(name string) string {
					if name == "DETENT_HUB_SECRET_KEYS" {
						return `{"2":"` + key + `"}`
					}
					return "2"
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			report, output := runSecretStatus(t, lookup, test.exit, "--database", path)
			if len(report.Databases) != 1 || report.Databases[0].Problem != "" {
				t.Fatalf("database status: %+v", report.Databases)
			}
			if report.ActiveVersion != 0 && report.ActiveVersion != 2 {
				t.Fatalf("active version=%d", report.ActiveVersion)
			}
			if !slices.Equal(report.ConfiguredVersions, mustStatusKeyring(t, lookup).Versions()) {
				t.Fatalf("configured versions=%v", report.ConfiguredVersions)
			}
			if !slices.Equal(report.Retirable, test.retirable) || !slices.Equal(report.MissingVersions, test.missing) {
				t.Fatalf("retirable=%v missing=%v", report.Retirable, report.MissingVersions)
			}
			wantProblems := 0
			if test.problem != "" {
				wantProblems = 2
				if test.name == "wrong key at same version" || test.name == "disabled with rows" {
					wantProblems = 4
				}
			}
			if len(report.Problems) != wantProblems || !reflect.DeepEqual(report.Problems, report.Databases[0].Problems) {
				t.Fatalf("problems=%+v", report.Problems)
			}
			for _, problem := range report.Problems {
				if problem.Problem != test.problem || problem.Database != path || problem.UpdatedAt != "2026-10-01T00:00:00Z" || problem.OrganizationID == "" || problem.Kind == "" {
					t.Fatalf("problem=%+v", problem)
				}
				if test.old && test.name != "disabled with rows" && problem.MasterKeyVersion != 1 {
					t.Fatalf("active row incorrectly reported: %+v", problem)
				}
			}
			count := 0
			tables := map[string]bool{}
			for _, group := range report.Databases[0].Counts {
				count += group.Count
				tables[group.Table] = true
			}
			if !test.empty && (count != 4 || !tables["project_secrets"] || !tables["organization_secrets"]) {
				t.Fatalf("counts=%+v", report.Databases[0].Counts)
			}
			for _, secret := range forbidden {
				if strings.Contains(output, secret) {
					t.Fatal("status exposed secret material")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("database changed: %v", err)
			}
			if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
				t.Fatalf("status created ownership lock: %v", err)
			}
		})
	}
}

func mustStatusKeyring(t *testing.T, lookup func(string) string) *hubsecrets.Keyring {
	t.Helper()
	keys, err := hubsecrets.FromEnvironment(lookup)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestHubSecretsStatusTenantRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, tenant := range []string{"a", "c"} {
		dir := filepath.Join(root, tenant)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		secretStatusFixture(t, filepath.Join(dir, "hub.db"), tenant == "a", false)
	}
	lookup := secretStatusEnvironment("2", "1", "2", "3")
	report, _ := runSecretStatus(t, lookup, 3, "--tenant-root", root)
	if len(report.Databases) != 2 || !slices.Equal(report.Retirable, []int{3}) || len(report.Problems) != 2 {
		t.Fatalf("tenant report=%+v", report)
	}
	broken := filepath.Join(root, "b")
	if err := os.Mkdir(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "hub.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	report, _ = runSecretStatus(t, lookup, 2, "--tenant-root", root)
	if len(report.Databases) != 3 || report.Databases[1].Problem == "" || len(report.Databases[2].Counts) == 0 || len(report.Retirable) != 0 {
		t.Fatalf("partial tenant report=%+v", report)
	}
	for index, tenant := range []string{"a", "b", "c"} {
		if report.Databases[index].Database != filepath.Join(root, tenant, "hub.db") {
			t.Fatal("tenant scan is not sorted")
		}
	}
}

func TestHubSecretsStatusInvalidInput(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		args      []string
		malformed bool
	}{
		{name: "no target"},
		{name: "both targets", args: []string{"--database", "unused.db", "--tenant-root", "unused"}},
		{name: "malformed keyring", args: []string{"--database", "unused.db"}, malformed: true},
		{name: "missing database", args: []string{"--database", filepath.Join(t.TempDir(), "missing.db")}},
		{name: "missing tables", args: []string{"--database", filepath.Join(t.TempDir(), "empty.db")}},
		{name: "missing tenant root", args: []string{"--tenant-root", filepath.Join(t.TempDir(), "missing")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := secretStatusEnvironment("2", "2")
			if test.malformed {
				lookup = func(string) string { return "secret-config-sentinel" }
			}
			if test.name == "missing tables" {
				if err := os.WriteFile(test.args[1], nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			report, output := runSecretStatus(t, lookup, 2, test.args...)
			if strings.Contains(output, "secret-config-sentinel") {
				t.Fatal("configuration leaked")
			}
			if report.Problem == "" && (len(report.Databases) != 1 || report.Databases[0].Problem == "") {
				t.Fatal("missing diagnostic")
			}
			if test.name == "missing database" {
				if _, err := os.Stat(test.args[1]); !os.IsNotExist(err) {
					t.Fatalf("status created database: %v", err)
				}
			}
		})
	}
}
