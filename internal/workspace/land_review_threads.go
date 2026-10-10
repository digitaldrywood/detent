package workspace

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ReviewConversationsRefusalText opens every protected-branch refusal caused by
// unresolved pull request review conversations. The landing owner routes such
// refusals to Rework so the listed findings reach the next attempt.
const ReviewConversationsRefusalText = "GitHub requires these review conversations to be resolved before the merge."

const (
	landingReviewThreadsQuery = `query NativeLandingReviewThreads($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$after){pageInfo{hasNextPage endCursor} nodes{id isResolved path line originalLine comments(first:50){nodes{body author{__typename login}}}}}}}}`
	landingReviewThreadReply  = `mutation NativeLandingReviewThreadReply($thread:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$thread,body:$body}){comment{id}}}`
	landingReviewThreadClose  = `mutation NativeLandingReviewThreadResolve($thread:ID!){resolveReviewThread(input:{threadId:$thread}){thread{isResolved}}}`
	landingReviewThreadPages  = 10
	landingReviewThreadLimit  = 20
	landingReviewThreadChars  = 600
)

var landingReviewThreadSurfaced = regexp.MustCompile(`<!-- detent:surfaced ([0-9a-f]{40}) -->`)

// ReviewConversationsRefusal reports whether a landing refusal names
// unresolved review conversations for Rework to address.
func ReviewConversationsRefusal(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), ReviewConversationsRefusalText)
}

type landingReviewThread struct {
	ID       string
	Path     string
	Line     int
	Body     string
	Author   string
	Bot      bool
	Surfaced []string
}

// resolveLandingReviewThreads answers a merge refused for unresolved review
// conversations. A bot conversation is resolved only when its own thread
// records that Detent surfaced it to Rework at an earlier head and a newer head
// has since been published. Every other unresolved conversation is returned as
// an actionable refusal, and each bot conversation is marked as surfaced at the
// current head so the next landing can prove it was handed to Rework. A nil
// result means the merge may be retried.
func resolveLandingReviewThreads(ctx context.Context, client GitHubRESTClient, repository string, number int, head string, refusal error) error {
	var landing *LandRefusal
	if !errors.As(refusal, &landing) || landing.Kind != LandRefusalProtected || !ReviewConversationsRefusal(landing.Reason) {
		return refusal
	}
	threads, err := readLandingReviewThreads(ctx, client, repository, number)
	if err != nil {
		return errors.Join(refusal, fmt.Errorf("read unresolved review conversations: %w", err))
	}
	resolved := 0
	var remaining []landingReviewThread
	for _, thread := range threads {
		if thread.Bot && surfacedBefore(thread, head) {
			if err := replyLandingReviewThread(ctx, client, thread.ID, "Addressed by the Detent Rework that published "+shortLandingHead(head)+"; resolving."); err == nil {
				if err := client.GraphQL(ctx, landingReviewThreadClose, map[string]any{"thread": thread.ID}, &struct{}{}); err == nil {
					resolved++
					continue
				}
			}
		}
		if thread.Bot && !surfacedAt(thread, head) {
			_ = replyLandingReviewThread(ctx, client, thread.ID, "Detent surfaced this conversation to Rework at "+shortLandingHead(head)+".\n\n<!-- detent:surfaced "+head+" -->")
		}
		remaining = append(remaining, thread)
	}
	if len(remaining) == 0 {
		if resolved == 0 {
			return refusal
		}
		return nil
	}
	return refuse(LandRefusalProtected, landingReviewThreadsReason(remaining, resolved))
}

func surfacedAt(thread landingReviewThread, head string) bool {
	for _, surfaced := range thread.Surfaced {
		if surfaced == head {
			return true
		}
	}
	return false
}

func surfacedBefore(thread landingReviewThread, head string) bool {
	for _, surfaced := range thread.Surfaced {
		if surfaced != head {
			return true
		}
	}
	return false
}

func readLandingReviewThreads(ctx context.Context, client GitHubRESTClient, repository string, number int) ([]landingReviewThread, error) {
	owner, name, _ := strings.Cut(repository, "/")
	var threads []landingReviewThread
	var after any
	for page := 0; page < landingReviewThreadPages; page++ {
		var result struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
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
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := client.GraphQL(ctx, landingReviewThreadsQuery, map[string]any{"owner": owner, "name": name, "number": number, "after": after}, &result); err != nil {
			return nil, err
		}
		if result.Repository == nil || result.Repository.PullRequest == nil {
			return nil, errors.New("the pull request was not found")
		}
		connection := result.Repository.PullRequest.ReviewThreads
		for _, node := range connection.Nodes {
			if node.IsResolved {
				continue
			}
			thread := landingReviewThread{ID: node.ID, Path: node.Path, Line: node.Line}
			if thread.Line == 0 {
				thread.Line = node.OriginalLine
			}
			for i, comment := range node.Comments.Nodes {
				if i == 0 {
					thread.Body = comment.Body
					if comment.Author != nil {
						thread.Author = comment.Author.Login
						thread.Bot = comment.Author.Typename == "Bot" || strings.HasSuffix(comment.Author.Login, "[bot]")
					}
					continue
				}
				for _, match := range landingReviewThreadSurfaced.FindAllStringSubmatch(comment.Body, -1) {
					thread.Surfaced = append(thread.Surfaced, match[1])
				}
			}
			threads = append(threads, thread)
		}
		if !connection.PageInfo.HasNextPage || connection.PageInfo.EndCursor == "" {
			return threads, nil
		}
		after = connection.PageInfo.EndCursor
	}
	return threads, nil
}

func replyLandingReviewThread(ctx context.Context, client GitHubRESTClient, thread, body string) error {
	return client.GraphQL(ctx, landingReviewThreadReply, map[string]any{"thread": thread, "body": body}, &struct{}{})
}

func landingReviewThreadsReason(threads []landingReviewThread, resolved int) string {
	var b strings.Builder
	b.WriteString(ReviewConversationsRefusalText)
	b.WriteString(" Address each finding in Rework. Detent resolves a bot conversation after a newer head is published, and human conversations need their author or a maintainer.")
	if resolved > 0 {
		fmt.Fprintf(&b, " %d bot conversation(s) surfaced to an earlier Rework were resolved.", resolved)
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
