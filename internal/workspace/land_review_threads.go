package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	landingReviewThreadsQuery = `query NativeLandingReviewThreads($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{id isResolved path line originalLine comments(first:1){nodes{body author{__typename login} originalCommit{oid}}}}}}}}`
	landingReviewThreadReply  = `mutation NativeLandingReviewThreadReply($thread:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$thread,body:$body}){comment{id}}}`
	landingReviewThreadClose  = `mutation NativeLandingReviewThreadResolve($thread:ID!){resolveReviewThread(input:{threadId:$thread}){thread{isResolved}}}`
	landingReviewThreadLimit  = 20
	landingReviewThreadChars  = 600
)

type landingReviewThread struct {
	ID     string
	Path   string
	Line   int
	Body   string
	Author string
	Bot    bool
	Commit string
}

// resolveLandingReviewThreads answers a merge refused for unresolved review
// conversations. Bot findings raised on an earlier head were given to the
// Rework that produced the current head, so the landing owner resolves them;
// every other unresolved conversation is returned as an actionable refusal
// for the next Rework. A nil result means the merge may be retried.
func resolveLandingReviewThreads(ctx context.Context, client GitHubRESTClient, repository string, number int, head string, refusal error) error {
	var landing *LandRefusal
	if !errors.As(refusal, &landing) || landing.Kind != LandRefusalReviewThreads {
		return refusal
	}
	threads, err := readLandingReviewThreads(ctx, client, repository, number)
	if err != nil {
		return errors.Join(refusal, fmt.Errorf("read unresolved review conversations: %w", err))
	}
	resolved := 0
	var remaining []landingReviewThread
	for _, thread := range threads {
		if !thread.Bot || thread.Commit == "" || thread.Commit == head {
			remaining = append(remaining, thread)
			continue
		}
		if err := closeLandingReviewThread(ctx, client, thread.ID, head); err != nil {
			remaining = append(remaining, thread)
			continue
		}
		resolved++
	}
	if len(remaining) == 0 {
		if resolved == 0 {
			return refusal
		}
		return nil
	}
	return refuse(LandRefusalReviewThreads, landingReviewThreadsReason(remaining, resolved))
}

func readLandingReviewThreads(ctx context.Context, client GitHubRESTClient, repository string, number int) ([]landingReviewThread, error) {
	owner, name, _ := strings.Cut(repository, "/")
	var result struct {
		Repository *struct {
			PullRequest *struct {
				ReviewThreads struct {
					Nodes []struct {
						ID           string `json:"id"`
						IsResolved   bool   `json:"isResolved"`
						Path         string `json:"path"`
						Line         int    `json:"line"`
						OriginalLine int    `json:"originalLine"`
						Comments     struct {
							Nodes []struct {
								Body   string `json:"body"`
								Author *struct {
									Typename string `json:"__typename"`
									Login    string `json:"login"`
								} `json:"author"`
								OriginalCommit *struct {
									OID string `json:"oid"`
								} `json:"originalCommit"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := client.GraphQL(ctx, landingReviewThreadsQuery, map[string]any{"owner": owner, "name": name, "number": number}, &result); err != nil {
		return nil, err
	}
	if result.Repository == nil || result.Repository.PullRequest == nil {
		return nil, errors.New("the pull request was not found")
	}
	var threads []landingReviewThread
	for _, node := range result.Repository.PullRequest.ReviewThreads.Nodes {
		if node.IsResolved {
			continue
		}
		thread := landingReviewThread{ID: node.ID, Path: node.Path, Line: node.Line}
		if thread.Line == 0 {
			thread.Line = node.OriginalLine
		}
		if len(node.Comments.Nodes) > 0 {
			comment := node.Comments.Nodes[0]
			thread.Body = comment.Body
			if comment.Author != nil {
				thread.Author = comment.Author.Login
				thread.Bot = comment.Author.Typename == "Bot" || strings.HasSuffix(comment.Author.Login, "[bot]")
			}
			if comment.OriginalCommit != nil {
				thread.Commit = comment.OriginalCommit.OID
			}
		}
		threads = append(threads, thread)
	}
	return threads, nil
}

func closeLandingReviewThread(ctx context.Context, client GitHubRESTClient, thread, head string) error {
	body := "Addressed by the Detent Rework that published " + shortLandingHead(head) + "; resolving."
	if err := client.GraphQL(ctx, landingReviewThreadReply, map[string]any{"thread": thread, "body": body}, &struct{}{}); err != nil {
		return err
	}
	return client.GraphQL(ctx, landingReviewThreadClose, map[string]any{"thread": thread}, &struct{}{})
}

func landingReviewThreadsReason(threads []landingReviewThread, resolved int) string {
	var b strings.Builder
	b.WriteString("GitHub requires these review conversations to be resolved before the merge. Address each finding in Rework; Detent resolves bot conversations once a new head is published, and human conversations need their author or a maintainer.")
	if resolved > 0 {
		fmt.Fprintf(&b, " %d bot conversation(s) raised on earlier heads were resolved.", resolved)
	}
	for i, thread := range threads {
		if i == landingReviewThreadLimit {
			fmt.Fprintf(&b, "\n- %d more unresolved conversation(s) on the pull request.", len(threads)-i)
			break
		}
		b.WriteString("\n- ")
		author := thread.Author
		if author == "" {
			author = "unknown"
		}
		b.WriteString(author)
		if thread.Path != "" {
			b.WriteString(" on ")
			b.WriteString(thread.Path)
			if thread.Line > 0 {
				fmt.Fprintf(&b, ":%d", thread.Line)
			}
		}
		b.WriteString(": ")
		body := strings.Join(strings.Fields(thread.Body), " ")
		if runes := []rune(body); len(runes) > landingReviewThreadChars {
			body = string(runes[:landingReviewThreadChars]) + "..."
		}
		b.WriteString(body)
	}
	return b.String()
}

func shortLandingHead(head string) string {
	if len(head) > 12 {
		return head[:12]
	}
	return head
}
