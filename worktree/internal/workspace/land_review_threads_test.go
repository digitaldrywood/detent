package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reviewThreadClient struct {
	pages       []string
	readErr     error
	failReply   string
	failResolve string
	cursors     []any
	mutations   []string
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
		if thread == c.failResolve {
			return errors.New("resolution refused")
		}
		kind = "resolve"
		if err := json.Unmarshal([]byte(`{"resolveReviewThread":{"thread":{"id":"`+thread+`","isResolved":true}}}`), out); err != nil {
			return err
		}
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
	overflowPages := make([]string, landingReviewThreadPages)
	for i := range overflowPages {
		overflowPages[i] = reviewThreadsPage(strconv.Itoa(i))
	}
	boundPage := func(value string) string {
		return strings.Replace(value, `"pullRequest":{`, `"pullRequest":{"number":7,"headRefOid":"`+head+`","baseRefName":"develop","headRepository":{"nameWithOwner":"example/repo"},"repository":{"nameWithOwner":"example/repo"},`, 1)
	}
	openBot := boundPage(reviewThreadsPage("", reviewThreadNode("T1", false, "example-bot", "Bot", "Preserve the accepted source", surfacedEarlier), reviewThreadNode("T2", false, "example-user", "User", "Human finding")))
	resolvedBot := boundPage(reviewThreadsPage("", reviewThreadNode("T1", true, "example-bot", "Bot", "Preserve the accepted source"), reviewThreadNode("T2", false, "example-user", "User", "Human finding")))
	for _, test := range []struct {
		name           string
		refusal        error
		client         *reviewThreadClient
		wantReason     []string
		wantMutations  []string
		wantCursors    int
		wantRework     bool
		resolve        bool
		disposition    string
		evidence       string
		thread         string
		authorityError bool
		wantSuccess    bool
	}{
		{
			name:    "explicit fixed evidence resolves only the selected bot conversation",
			resolve: true, disposition: "fixed", evidence: "Published source.go:17 now retains the accepted head; existing regression passes on this head.", thread: "T1", wantSuccess: true,
			client:        &reviewThreadClient{pages: []string{openBot, openBot, resolvedBot}},
			wantMutations: []string{"reply:T1", "resolve:T1"}, wantCursors: 3,
		},
		{
			name:    "explicit dismissal can resolve an unchanged reviewed source",
			resolve: true, disposition: "dismissed", evidence: "source.go:17 already checks the immutable head; the finding assumes a different code path.", thread: "T1", wantSuccess: true,
			client:        &reviewThreadClient{pages: []string{openBot, openBot, resolvedBot}},
			wantMutations: []string{"reply:T1", "resolve:T1"}, wantCursors: 3,
		},
		{
			name:    "surfacing alone is never disposition evidence",
			resolve: true, disposition: "fixed", thread: "T1", client: &reviewThreadClient{},
			wantReason: []string{"requires a thread"},
		},
		{
			name:    "human conversations remain protected from the bot resolution operation",
			resolve: true, disposition: "fixed", evidence: "A claimed correction", thread: "T2", client: &reviewThreadClient{pages: []string{openBot}}, wantCursors: 1,
			wantReason: []string{"human review owner"},
		},
		{
			name:    "an unrelated conversation ID cannot be resolved",
			resolve: true, disposition: "dismissed", evidence: "A claimed dismissal", thread: "OTHER", client: &reviewThreadClient{pages: []string{openBot}}, wantCursors: 1,
			wantReason: []string{"not part of the reviewed pull request"},
		},
		{
			name:    "lost current source authority performs no review operation",
			resolve: true, disposition: "fixed", evidence: "A claimed correction", thread: "T1", authorityError: true, client: &reviewThreadClient{},
			wantReason: []string{"source authority lost"},
		},
		{
			name:    "a moved head after the reply cannot resolve the conversation",
			resolve: true, disposition: "fixed", evidence: "A correction on the original published head", thread: "T1", client: &reviewThreadClient{pages: []string{openBot, strings.ReplaceAll(openBot, head, earlier)}}, wantCursors: 2,
			wantMutations: []string{"reply:T1"}, wantReason: []string{"no longer belong"},
		},
		{
			name:    "resolution rejection does not report success",
			resolve: true, disposition: "fixed", evidence: "A correction on the published head", thread: "T1", client: &reviewThreadClient{pages: []string{openBot, openBot}, failResolve: "T1"}, wantCursors: 2,
			wantMutations: []string{"reply:T1"}, wantReason: []string{"resolution refused"},
		},

		{
			name:          "preflight uses the existing actionable bot refusal and surfacing owner",
			client:        &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "example-bot", "Bot", "P2 finding"))}},
			wantReason:    []string{ReviewConversationsRefusalText, "P2 finding"},
			wantMutations: []string{"surface:T1"},
			wantCursors:   1,
			wantRework:    true,
		},
		{
			name:        "preflight preserves a human finding without mutation",
			client:      &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "example-user", "User", "Human finding"))}},
			wantReason:  []string{ReviewConversationsRefusalText, "Human finding"},
			wantCursors: 1,
			wantRework:  true,
		},
		{
			name:        "a missing pagination cursor never approves a partial read",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: []string{strings.Replace(reviewThreadsPage(""), `"hasNextPage":false`, `"hasNextPage":true`, 1)}},
			wantReason:  []string{"did not advance its pagination cursor"},
			wantCursors: 1,
			wantRework:  true,
		},
		{
			name:        "a repeated pagination cursor never approves a partial read",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: []string{reviewThreadsPage("c1"), reviewThreadsPage("c1")}},
			wantReason:  []string{"did not advance its pagination cursor"},
			wantCursors: 2,
			wantRework:  true,
		},
		{
			name:        "the pagination limit never approves a partial read",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: overflowPages},
			wantReason:  []string{"exceeded the pagination limit"},
			wantCursors: landingReviewThreadPages,
			wantRework:  true,
		},
		{
			name:    "other protected refusals pass through untouched",
			refusal: refuse(LandRefusalProtected, "required reviews"),
			client:  &reviewThreadClient{},
		},
		{
			name:          "a bot finding surfaced at an earlier head stays actionable without disposition proof",
			refusal:       conversation,
			client:        &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "P2 finding", surfacedEarlier), reviewThreadNode("T0", true, "chatgpt-codex-connector", "Bot", "done"))}},
			wantReason:    []string{ReviewConversationsRefusalText, "P2 finding"},
			wantMutations: []string{"surface:T1"},
			wantCursors:   1,
			wantRework:    true,
		},
		{
			name:          "a bot finding never surfaced is surfaced, not resolved",
			refusal:       conversation,
			client:        &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "Honor partial baselines"))}},
			wantReason:    []string{ReviewConversationsRefusalText, "chatgpt-codex-connector on web/app.ts:7: unaddressed: Honor partial baselines"},
			wantMutations: []string{"surface:T1"},
			wantCursors:   1,
			wantRework:    true,
		},
		{
			name:        "a bot finding already surfaced at this head stays actionable without another reply",
			refusal:     conversation,
			client:      &reviewThreadClient{pages: []string{reviewThreadsPage("", reviewThreadNode("T1", false, "chatgpt-codex-connector", "Bot", "Still open", surfacedNow))}},
			wantReason:  []string{ReviewConversationsRefusalText, "Still open"},
			wantCursors: 1,
			wantRework:  true,
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
			wantReason:  []string{"example-user on web/app.ts:7: unaddressed: Please rename this"},
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
			wantReason:  []string{ReviewConversationsRefusalText, "P1 finding"},
			wantCursors: 1,
			wantRework:  true,
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
			var err error
			if test.resolve {
				opts := LandOptions{Repository: "https://github.com/example/repo", HeadSHA: head, TargetBranch: "develop", External: &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/example/repo/pull/7"}, Authorize: func(context.Context) error {
					if test.authorityError {
						return errors.New("source authority lost")
					}
					return nil
				}}
				err = ResolveReviewConversation(t.Context(), test.client, opts, test.thread, test.disposition, test.evidence)
				if (err == nil) != test.wantSuccess {
					t.Fatalf("ResolveReviewConversation()=%v, success=%t", err, test.wantSuccess)
				}
			} else {
				err = resolveLandingReviewThreads(context.Background(), test.client, "example/repo", 7, head, test.refusal, LandOptions{})
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected {
					t.Fatalf("resolveLandingReviewThreads() = %v, want a protected refusal", err)
				}
				if got := ReviewConversationsRefusal(refusal.Reason); got != test.wantRework {
					t.Fatalf("ReviewConversationsRefusal(%q) = %t, want %t", refusal.Reason, got, test.wantRework)
				}
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
			if !test.resolve && test.wantCursors == 2 && test.client.cursors[1] != "c1" {
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
