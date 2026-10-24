package coordination

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitHubRefStoreBootstrapsAndComparesHead(t *testing.T) {
	t.Parallel()
	client := newGitHubRefTestClient()
	store, err := NewGitHubRefStore(GitHubRefConfig{Repository: "example/coordination", Branch: "scheduler", Client: client})
	if err != nil {
		t.Fatalf("NewGitHubRefStore() error = %v", err)
	}

	if _, swapped, err := store.CompareAndSwap(t.Context(), "projects/one.json", "", []byte("one")); err != nil || swapped {
		t.Fatalf("bootstrap CompareAndSwap() = swapped %t, error %v", swapped, err)
	}
	record, found, err := store.Get(t.Context(), "projects/one.json")
	if err != nil || found || record.Version != "base" {
		t.Fatalf("Get() after bootstrap = %#v, %t, %v", record, found, err)
	}
	first, swapped, err := store.CompareAndSwap(t.Context(), "projects/one.json", record.Version, []byte("one"))
	if err != nil || !swapped || first.Version != "commit-1" {
		t.Fatalf("first CompareAndSwap() = %#v, %t, %v", first, swapped, err)
	}
	if _, swapped, err := store.CompareAndSwap(t.Context(), "projects/one.json", record.Version, []byte("two")); err != nil || swapped {
		t.Fatalf("stale CompareAndSwap() = swapped %t, error %v", swapped, err)
	}
	got, found, err := store.Get(t.Context(), "projects/one.json")
	if err != nil || !found || string(got.Value) != "one" || got.Version != "commit-1" {
		t.Fatalf("Get() = %#v, %t, %v", got, found, err)
	}
}

func TestGitHubRefStoreRejectsUnsafeKeys(t *testing.T) {
	t.Parallel()
	store, err := NewGitHubRefStore(GitHubRefConfig{Repository: "example/coordination", Branch: "scheduler", Client: newGitHubRefTestClient()})
	if err != nil {
		t.Fatalf("NewGitHubRefStore() error = %v", err)
	}
	if _, _, err := store.Get(t.Context(), "../outside"); !errors.Is(err, ErrInvalidGitHubRefConfig) {
		t.Fatalf("Get() error = %v, want ErrInvalidGitHubRefConfig", err)
	}
}

func TestGitHubRefStoreCoordinationWritesUseOneGraphQLCall(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		seedKey  string
		wantREST int
		write    func(context.Context, *GitHubRefStore) error
	}{
		{name: "schedule ownership renewal", seedKey: "schedule/lease.json", wantREST: 3, write: func(ctx context.Context, store *GitHubRefStore) error {
			record, _, err := store.Get(ctx, "schedule/lease.json")
			if err != nil {
				return err
			}
			_, swapped, err := store.CompareAndSwap(ctx, "schedule/lease.json", record.Version, []byte(`{"owner":"one"}`))
			if err == nil && !swapped {
				return errors.New("renewal did not swap")
			}
			return err
		}},
		{name: "lane write publication", wantREST: 2, write: func(ctx context.Context, store *GitHubRefStore) error {
			return PublishLaneWrite(ctx, store, "project", LaneWrite{InstanceIdentity: "one", Issue: "issue", To: "Done", Reason: "test", FenceToken: 1, WrittenAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := newGitHubRefTestClient()
			client.branch, client.head = true, client.defaultO
			if test.seedKey != "" {
				client.values[test.seedKey] = `{"owner":"one"}`
				client.modified[test.seedKey] = time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)
			}
			store, err := NewGitHubRefStore(GitHubRefConfig{Repository: "example/coordination", Branch: "scheduler", Client: client})
			if err != nil {
				t.Fatal(err)
			}
			if err := test.write(t.Context(), store); err != nil {
				t.Fatal(err)
			}
			if client.graphqlCalls != 1 {
				t.Fatalf("GraphQL calls = %d, want one commit", client.graphqlCalls)
			}
			if client.restCalls != test.wantREST {
				t.Fatalf("REST calls = %d, want %d reads without duplicate CAS read", client.restCalls, test.wantREST)
			}
		})
	}
}

type githubRefTestClient struct {
	mu           sync.Mutex
	branch       bool
	head         string
	defaultO     string
	values       map[string]string
	modified     map[string]time.Time
	commits      int
	graphqlCalls int
	restCalls    int
}

func newGitHubRefTestClient() *githubRefTestClient {
	return &githubRefTestClient{defaultO: "base", values: map[string]string{}, modified: map[string]time.Time{}}
}

func (c *githubRefTestClient) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.graphqlCalls++
	switch {
	case strings.Contains(query, "DetentCoordinationCommit"):
		input := variables["input"].(map[string]any)
		expected := input["expectedHeadOid"].(string)
		if expected != c.head {
			return errors.New("expected head does not match")
		}
		addition := input["fileChanges"].(map[string]any)["additions"].([]map[string]any)[0]
		key := addition["path"].(string)
		encoded := addition["contents"].(string)
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		c.commits++
		c.head = fmt.Sprintf("commit-%d", c.commits)
		modified := time.Date(2026, 8, 24, 12, c.commits, 0, 0, time.UTC)
		c.values[key] = string(value)
		c.modified[key] = modified
		return decodeGitHubRefTestResponse(out, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{"oid": c.head, "committedDate": modified}}})
	default:
		return errors.New("unexpected graphql operation")
	}
}

type githubRefNotFound struct{}

func (githubRefNotFound) Error() string   { return "not found" }
func (githubRefNotFound) HTTPStatus() int { return http.StatusNotFound }

func (c *githubRefTestClient) REST(_ context.Context, method, path string, body any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restCalls++
	parsed, err := url.Parse(path)
	if err != nil {
		return err
	}
	switch {
	case method == http.MethodGet && parsed.Path == "/repos/example/coordination":
		return decodeGitHubRefTestResponse(out, map[string]string{"default_branch": "main"})
	case method == http.MethodGet && parsed.Path == "/repos/example/coordination/git/ref/heads/main":
		return decodeGitHubRefTestResponse(out, map[string]any{"object": map[string]string{"sha": c.defaultO, "type": "commit"}})
	case method == http.MethodGet && parsed.Path == "/repos/example/coordination/git/ref/heads/scheduler":
		if !c.branch {
			return githubRefNotFound{}
		}
		return decodeGitHubRefTestResponse(out, map[string]any{"object": map[string]string{"sha": c.head, "type": "commit"}})
	case method == http.MethodPost && parsed.Path == "/repos/example/coordination/git/refs":
		if c.branch {
			return errors.New("reference already exists")
		}
		c.branch, c.head = true, c.defaultO
		return nil
	case method == http.MethodGet && strings.HasPrefix(parsed.Path, "/repos/example/coordination/contents/"):
		if parsed.Query().Get("ref") != c.head {
			return fmt.Errorf("contents ref = %q, want head %q", parsed.Query().Get("ref"), c.head)
		}
		key := strings.TrimPrefix(parsed.Path, "/repos/example/coordination/contents/")
		value, ok := c.values[key]
		if !ok {
			return githubRefNotFound{}
		}
		return decodeGitHubRefTestResponse(out, map[string]string{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(value))})
	case method == http.MethodGet && parsed.Path == "/repos/example/coordination/commits":
		if parsed.Query().Get("sha") != c.head || parsed.Query().Get("per_page") != "1" {
			return fmt.Errorf("commits query = %q, want head and one commit", parsed.RawQuery)
		}
		key := parsed.Query().Get("path")
		return decodeGitHubRefTestResponse(out, []map[string]any{{"commit": map[string]any{"committer": map[string]any{"date": c.modified[key]}}}})
	default:
		return fmt.Errorf("unexpected REST operation %s %s", method, path)
	}
}

func decodeGitHubRefTestResponse(out any, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
