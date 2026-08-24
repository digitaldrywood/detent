package coordination

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

type githubRefTestClient struct {
	mu       sync.Mutex
	branch   bool
	head     string
	defaultO string
	values   map[string]string
	modified map[string]time.Time
	commits  int
}

func newGitHubRefTestClient() *githubRefTestClient {
	return &githubRefTestClient{defaultO: "base", values: map[string]string{}, modified: map[string]time.Time{}}
}

func (c *githubRefTestClient) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case strings.Contains(query, "DetentCoordinationState"):
		key, _ := variables["path"].(string)
		repository := map[string]any{
			"id":               "R_1",
			"defaultBranchRef": map[string]any{"target": map[string]any{"oid": c.defaultO}},
		}
		if c.branch {
			history := []map[string]any{}
			if modified := c.modified[key]; !modified.IsZero() {
				history = append(history, map[string]any{"committedDate": modified})
			}
			repository["ref"] = map[string]any{"target": map[string]any{"oid": c.head, "history": map[string]any{"nodes": history}}}
			if value, ok := c.values[key]; ok {
				repository["object"] = map[string]any{"oid": "blob", "text": value}
			}
		}
		return decodeGitHubRefTestResponse(out, map[string]any{"repository": repository})
	case strings.Contains(query, "DetentCoordinationCreateRef"):
		if c.branch {
			return errors.New("reference already exists")
		}
		c.branch = true
		c.head = c.defaultO
		return decodeGitHubRefTestResponse(out, map[string]any{"createRef": map[string]any{"ref": map[string]any{"id": "REF_1"}}})
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

func decodeGitHubRefTestResponse(out any, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
