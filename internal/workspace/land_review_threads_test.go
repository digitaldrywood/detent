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
	pages     []string
	readErr   error
	failReply string
	cursors   []any
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
		c.cursors = append(c.cursors, variables["after"])
		return json.Unmarshal([]byte(c.pages[len(c.cursors)-1]), out)
	}
	thread, _ := variables["thread"].(string)
	if thread == c.failReply {
		return errors.New("mutation refused")
	}
	kind := "reply"
	if strings.Contains(query, "resolveReviewThread") {
		kind = "resolve"
	} else if strings.Contains(variables["body"].(string), "detent:surfaced") {
		kind = "surface"
	}
	c.mutations = append(c.mutations, kind+":"+thread)
	return nil
}

func reviewThreadsPage(next string, nodes ...string) string {
	return `{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":` + map[bool]string{true: "true", false: "false"}[next != ""] + `,"endCursor":"` + next + `"},"nodes":[` + strings.Join(nodes, ",") + `]}}}}`
}

func reviewThreadNode(id string, resolved bool, author, typename, body string, replies ...string) string {
	comments := []any{map[string]any{"body": body, "author": map[string]any{"__typename": typename, "login": author}}}
	for _, reply := range replies {
		comments = append(comments, map[string]any{"body": reply, "author": map[string]any{"__typename": "User", "login": "example-user"}})
	}
	encoded, _ := json.Marshal(map[string]any{"id": id, "isResolved": resolved, "path": "web/app.ts", "line": 7, "comments": map[string]any{"nodes": comments}})
	return string(encoded)
}

func TestResolveLandingReviewThreads(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	const earlier = "2222222222222222222222222222222222222222"
	surfacedEarlier := "Detent surfaced this conversation to Rework.\n\n<!-- detent:surfaced " + earlier + " -->"
	surfacedNow := "<!-- detent:surfaced " + head + " -->"
	conversation := refuse(LandRefusalProtected, ReviewConversationsRefusalText+" GitHub refused the merge")
	for _, test := range []struct {
		name          string
		refusal       error
		client        *reviewThreadClient
		wantReason    []string
		wantMutations []string
		wantCursors   int
		wantRework    bool
	}{
		{
			name:    "other protected refusals pass through untouched",
			refusal: refuse(LandRefusalProtected, "required reviews"),
			client:  &reviewThreadClient{},
		},
		{
			name:        "a bot finding surfaced at an earlier head stays actionable without disposition proof",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "P2 finding", surfacedEarlier), reviewThreadNode("T0", true, "chatgpt-codex-connector", "Bot", "done"))}},
			wantReason:  []string{"operator disposition", "explicitly resume", "P2 finding"},
			wantCursors: 1,
		},
		{
			name:          "a bot finding never surfaced is surfaced, not resolved",
			refusal:       conversation,
			client:        &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "Honor partial baselines"))}},
			wantReason:    []string{ReviewConversationsRefusalText, "chatgpt-codex-connector on web/app.ts:7: Honor partial baselines"},
			wantMutations: []string{"surface:T1"},
			wantCursors:   1,
			wantRework:    true,
		},
		{
			name:        "a bot finding already surfaced at this head stays actionable without another reply",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "Still open", surfacedNow))}},
			wantReason:  []string{"operator disposition", "explicitly resume", "Still open"},
			wantCursors: 1,
		},
		{
			name:          "a new bot finding keeps Rework available alongside a prior surfaced finding",
			refusal:       conversation,
			client:        &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "Prior finding", surfacedEarlier), reviewThreadNode("T2", false, "chatgpt-codex-connector", "Bot", "New finding"))}},
			wantReason:    []string{ReviewConversationsRefusalText, "Prior finding", "New finding"},
			wantMutations: []string{"surface:T1", "surface:T2"},
			wantCursors:   1,
			wantRework:    true,
		},
		{
			name:          "a human conversation preserves existing Rework alongside a prior surfaced bot finding",
			refusal:       conversation,
			client:        &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "Prior finding", surfacedEarlier), reviewThreadNode("T2", false, "reviewer", "User", "Human finding", surfacedEarlier), reviewThreadNode("T3", false, "chatgpt-codex-connector", "Bot", "Current head finding", surfacedNow))}},
			wantReason:    []string{ReviewConversationsRefusalText, "Prior finding", "Human finding", "Current head finding"},
			wantMutations: []string{"surface:T1"},
			wantCursors:   1,
			wantRework:    true,
		},
		{
			name:        "human conversations are surfaced in the refusal and never resolved",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "example-user", "User", "Please rename this", surfacedEarlier))}},
			wantReason:  []string{"example-user on web/app.ts:7: Please rename this"},
			wantCursors: 1,
			wantRework:  true,
		},
		{
			name:          "every page of the thread connection is read",
			refusal:       conversation,
			client:        &reviewThreadClient{pages: []string{reviewThreadsPage("c1", reviewThreadNode("T0", true, "chatgpt-codex-connector", "Bot", "old")), reviewThreadsPage("", reviewThreadNode("T9", false, "chatgpt-codex-connector", "Bot", "Second page finding"))}},
			wantReason:    []string{"Second page finding"},
			wantMutations: []string{"surface:T9"},
			wantCursors:   2,
			wantRework:    true,
		},
		{
			name:        "an earlier surfacing marker does not resolve a finding already surfaced at this head",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "P1 finding", surfacedEarlier, surfacedNow))}},
			wantReason:  []string{"operator disposition", "explicitly resume", "P1 finding"},
			wantCursors: 1,
		},
		{
			name:        "a failed surface reply keeps the finding and reports the error",
			refusal:     conversation,
			client:      &reviewThreadClient{failReply: "T1", pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "P1 finding"))}},
			wantReason:  []string{"P1 finding", "surfacing reply failed", "mark review conversation as surfaced: mutation refused"},
			wantCursors: 1,
			wantRework:  true,
		},
		{
			name:       "an unreadable thread list keeps the GitHub refusal",
			refusal:    conversation,
			client:     &reviewThreadClient{readErr: &github.StatusError{StatusCode: http.StatusBadGateway}},
			wantReason: []string{"GitHub refused the merge"},
			wantRework: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := resolveLandingReviewThreads(context.Background(), test.client, "example/repo", 7, head, test.refusal)
			var refusal *LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected {
				t.Fatalf("resolveLandingReviewThreads() = %v, want a protected refusal", err)
			}
			if got := ReviewConversationsRefusal(refusal.Reason); got != test.wantRework {
				t.Fatalf("ReviewConversationsRefusal(%q) = %t, want %t", refusal.Reason, got, test.wantRework)
			}
			for _, want := range test.wantReason {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q missing %q", err.Error(), want)
				}
			}
			if strings.Join(test.client.mutations, ",") != strings.Join(test.wantMutations, ",") {
				t.Fatalf("mutations = %v, want %v", test.client.mutations, test.wantMutations)
			}
			if len(test.client.cursors) != test.wantCursors {
				t.Fatalf("pages read = %d, want %d", len(test.client.cursors), test.wantCursors)
			}
			if test.wantCursors == 2 && test.client.cursors[1] != "c1" {
				t.Fatalf("second page cursor = %v, want c1", test.client.cursors[1])
			}
		})
	}
}

func TestGitHubLandingRESTConversationRefusal(t *testing.T) {
	for _, test := range []struct {
		name         string
		message      string
		conversation bool
	}{
		{name: "ruleset conversation resolution", message: "Repository rule violations found\n\nA conversation must be resolved before this pull request can be merged.", conversation: true},
		{name: "branch protection reviews stay generic", message: "Branch protection requires reviews"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"message": test.message})
			client := &statusRESTClient{err: &github.StatusError{StatusCode: http.StatusMethodNotAllowed, Body: string(body)}}
			err := githubLandingREST(context.Background(), client, http.MethodPut, "repos/example/repo/pulls/7/merge", nil, nil)
			var refusal *LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected || ReviewConversationsRefusal(refusal.Reason) != test.conversation {
				t.Fatalf("githubLandingREST() = %v, want protected refusal with conversation=%v", err, test.conversation)
			}
		})
	}
}

type statusRESTClient struct{ err error }

func (c *statusRESTClient) REST(context.Context, string, string, any, any) error { return c.err }
func (c *statusRESTClient) GraphQL(context.Context, string, map[string]any, any) error {
	return errors.New("unexpected GraphQL call")
}
