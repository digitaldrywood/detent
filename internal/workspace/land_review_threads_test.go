package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector/github"
)

type reviewThreadClient struct {
	threads   string
	readErr   error
	failClose string
	mutations []string
}

func (c *reviewThreadClient) REST(context.Context, string, string, any, any) error {
	return errors.New("unexpected REST call")
}

func (c *reviewThreadClient) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	if strings.HasPrefix(query, "query") {
		if c.readErr != nil {
			return c.readErr
		}
		return json.Unmarshal([]byte(c.threads), out)
	}
	thread, _ := variables["thread"].(string)
	if thread == c.failClose {
		return errors.New("mutation refused")
	}
	kind := "reply"
	if strings.Contains(query, "resolveReviewThread") {
		kind = "resolve"
	}
	c.mutations = append(c.mutations, kind+":"+thread)
	return nil
}

func reviewThreadsResponse(nodes ...string) string {
	return `{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` + strings.Join(nodes, ",") + `]}}}}`
}

func reviewThreadNode(id string, resolved bool, author, typename, commit, body string) string {
	encoded, _ := json.Marshal(map[string]any{
		"id": id, "isResolved": resolved, "path": "web/app.ts", "line": 7,
		"comments": map[string]any{"nodes": []any{map[string]any{
			"body":           body,
			"author":         map[string]any{"__typename": typename, "login": author},
			"originalCommit": map[string]any{"oid": commit},
		}}},
	})
	return string(encoded)
}

func TestResolveLandingReviewThreads(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	const earlier = "2222222222222222222222222222222222222222"
	conversation := refuse(LandRefusalReviewThreads, "GitHub refused the merge")
	for _, test := range []struct {
		name          string
		refusal       error
		client        *reviewThreadClient
		wantRetry     bool
		wantKind      string
		wantReason    []string
		wantMutations []string
	}{
		{
			name:     "other refusals pass through untouched",
			refusal:  refuse(LandRefusalProtected, "required reviews"),
			client:   &reviewThreadClient{},
			wantKind: LandRefusalProtected,
		},
		{
			name:          "bot findings from an earlier head are resolved and the merge retries",
			refusal:       conversation,
			client:        &reviewThreadClient{threads: reviewThreadsResponse(reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", earlier, "P2 finding"), reviewThreadNode("T0", true, "chatgpt-codex-connector", "Bot", earlier, "done"))},
			wantRetry:     true,
			wantMutations: []string{"reply:T1", "resolve:T1"},
		},
		{
			name:       "bot findings on the current head reach Rework",
			refusal:    conversation,
			client:     &reviewThreadClient{threads: reviewThreadsResponse(reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", head, "Honor partial baselines"))},
			wantKind:   LandRefusalReviewThreads,
			wantReason: []string{"chatgpt-codex-connector on web/app.ts:7: Honor partial baselines"},
		},
		{
			name:          "human conversations are surfaced and never resolved",
			refusal:       conversation,
			client:        &reviewThreadClient{threads: reviewThreadsResponse(reviewThreadNode("T1", false, "corylanou", "User", earlier, "Please rename this"), reviewThreadNode("T2", false, "reviewer[bot]", "User", earlier, "old finding"))},
			wantKind:      LandRefusalReviewThreads,
			wantReason:    []string{"corylanou on web/app.ts:7: Please rename this", "1 bot conversation(s) raised on earlier heads were resolved"},
			wantMutations: []string{"reply:T2", "resolve:T2"},
		},
		{
			name:       "a failed resolution keeps the finding actionable",
			refusal:    conversation,
			client:     &reviewThreadClient{failClose: "T1", threads: reviewThreadsResponse(reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", earlier, "P1 finding"))},
			wantKind:   LandRefusalReviewThreads,
			wantReason: []string{"P1 finding"},
		},
		{
			name:       "an unreadable thread list keeps the GitHub refusal",
			refusal:    conversation,
			client:     &reviewThreadClient{readErr: &github.StatusError{StatusCode: http.StatusBadGateway}},
			wantKind:   LandRefusalReviewThreads,
			wantReason: []string{"GitHub refused the merge"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := resolveLandingReviewThreads(context.Background(), test.client, "example/repo", 7, head, test.refusal)
			if test.wantRetry {
				if err != nil {
					t.Fatalf("resolveLandingReviewThreads() = %v, want merge retry", err)
				}
			} else {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != test.wantKind {
					t.Fatalf("resolveLandingReviewThreads() = %v, want %s refusal", err, test.wantKind)
				}
				for _, want := range test.wantReason {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal %q missing %q", err.Error(), want)
					}
				}
			}
			if strings.Join(test.client.mutations, ",") != strings.Join(test.wantMutations, ",") {
				t.Fatalf("mutations = %v, want %v", test.client.mutations, test.wantMutations)
			}
		})
	}
}

func TestGitHubLandingRESTConversationRefusal(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		want    string
	}{
		{name: "ruleset conversation resolution", message: "Repository rule violations found\n\nA conversation must be resolved before this pull request can be merged.", want: LandRefusalReviewThreads},
		{name: "branch protection reviews stay protected", message: "Branch protection requires reviews", want: LandRefusalProtected},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"message": test.message})
			client := &statusRESTClient{err: &github.StatusError{StatusCode: http.StatusMethodNotAllowed, Body: string(body)}}
			err := githubLandingREST(context.Background(), client, http.MethodPut, "repos/example/repo/pulls/7/merge", nil, nil)
			var refusal *LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != test.want {
				t.Fatalf("githubLandingREST() = %v, want %s", err, test.want)
			}
		})
	}
}

type statusRESTClient struct{ err error }

func (c *statusRESTClient) REST(context.Context, string, string, any, any) error { return c.err }
func (c *statusRESTClient) GraphQL(context.Context, string, map[string]any, any) error {
	return errors.New("unexpected GraphQL call")
}
