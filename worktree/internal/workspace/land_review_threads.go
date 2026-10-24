package workspace

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ReviewConversationsRefusalText opens every protected-branch refusal caused by
// unresolved pull request review conversations. The landing owner routes such
// refusals to Rework so the listed findings reach the next attempt.
const ReviewConversationsRefusalText = "GitHub requires these review conversations to be resolved before the merge."

const (
	landingReviewThreadsQuery  = `query NativeLandingReviewThreads($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){number headRefOid baseRefName headRepository{nameWithOwner} repository{nameWithOwner} reviewThreads(first:100,after:$after){pageInfo{hasNextPage endCursor} nodes{id isResolved path line originalLine comments(first:50){pageInfo{hasNextPage} nodes{body author{__typename login}}}}}}}}`
	landingReviewThreadReply   = `mutation NativeLandingReviewThreadReply($thread:ID!,$body:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$thread,body:$body}){comment{id}}}`
	landingReviewThreadResolve = `mutation NativeLandingReviewThreadResolve($thread:ID!){resolveReviewThread(input:{threadId:$thread}){thread{id isResolved}}}`
	landingReviewThreadPages   = 10
	landingReviewThreadLimit   = 20
	landingReviewThreadChars   = 600
)

var landingReviewThreadSurfaced = regexp.MustCompile(`<!-- detent:surfaced ([0-9a-f]{40}) -->`)

// ReviewConversationsResolvedText records that a Rework resolved the review
// conversations a landing refusal named, releasing that refusal's landing hold.
const ReviewConversationsResolvedText = "Rework resolved the review conversations that refused this version's landing."

// ReviewConversationsResolved reports whether a lane transition records that
// resolution.
func ReviewConversationsResolved(detail string) bool {
	return strings.Contains(detail, ReviewConversationsResolvedText)
}

// ReviewConversationsRefusal reports whether a landing refusal names
// unresolved review conversations for Rework to address.
func ReviewConversationsRefusal(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), ReviewConversationsRefusalText)
}

type ReviewConversation struct {
	ID           string                      `json:"id"`
	Path         string                      `json:"path"`
	Line         int                         `json:"line"`
	Body         string                      `json:"body"`
	Author       string                      `json:"author"`
	Bot          bool                        `json:"bot"`
	Surfaced     []string                    `json:"-"`
	Replies      []ReviewConversationComment `json:"replies,omitempty"`
	MoreComments bool                        `json:"more_comments"`
}

type ReviewConversationComment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

func resolveLandingReviewThreads(ctx context.Context, client GitHubRESTClient, repository string, number int, head string, refusal error) error {
	var landing *LandRefusal
	if refusal != nil && (!errors.As(refusal, &landing) || landing.Kind != LandRefusalProtected || !ReviewConversationsRefusal(landing.Reason)) {
		return refusal
	}
	threads, err := readLandingReviewThreads(ctx, client, repository, number, nil)
	if err != nil {
		return errors.Join(refusal, fmt.Errorf("read unresolved review conversations: %w", err))
	}
	for i, thread := range threads {
		if thread.Bot && !surfacedAt(thread, head) {
			if replyErr := replyLandingReviewThread(ctx, client, thread.ID, "Detent surfaced this conversation to Rework at "+shortLandingHead(head)+".\n\n<!-- detent:surfaced "+head+" -->"); replyErr != nil {
				threads[i].Body += "\n(surfacing reply failed)"
				err = errors.Join(err, fmt.Errorf("mark review conversation as surfaced: %w", replyErr))
			}
		}
	}
	if len(threads) == 0 {
		return refusal
	}
	return errors.Join(refuse(LandRefusalProtected, landingReviewThreadsReason(threads)), err)
}

func surfacedAt(thread ReviewConversation, head string) bool {
	for _, surfaced := range thread.Surfaced {
		if surfaced == head {
			return true
		}
	}
	return false
}

func readLandingReviewThreads(ctx context.Context, client GitHubRESTClient, repository string, number int, expected *LandOptions) ([]ReviewConversation, error) {
	owner, name, _ := strings.Cut(repository, "/")
	var threads []ReviewConversation
	var after any
	for range landingReviewThreadPages {
		var result struct {
			Repository *struct {
				PullRequest *struct {
					Number         int    `json:"number"`
					Head           string `json:"headRefOid"`
					Base           string `json:"baseRefName"`
					HeadRepository struct {
						Name string `json:"nameWithOwner"`
					} `json:"headRepository"`
					Repository struct {
						Name string `json:"nameWithOwner"`
					} `json:"repository"`
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
								PageInfo struct {
									HasNextPage bool `json:"hasNextPage"`
								} `json:"pageInfo"`
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
		pull := result.Repository.PullRequest
		if expected != nil && (pull.Number != number || pull.Head != expected.HeadSHA || pull.HeadRepository.Name != repository || pull.Repository.Name != repository || expected.TargetBranch != "" && pull.Base != expected.TargetBranch) {
			return nil, refuse(LandRefusalHeadMoved, "the review conversations no longer belong to the reviewed pull request head")
		}
		connection := result.Repository.PullRequest.ReviewThreads
		for _, node := range connection.Nodes {
			if node.IsResolved {
				continue
			}
			thread := ReviewConversation{ID: node.ID, Path: node.Path, Line: node.Line, MoreComments: node.Comments.PageInfo.HasNextPage}
			if thread.Line == 0 {
				thread.Line = node.OriginalLine
			}
			for i, comment := range node.Comments.Nodes {
				author := ""
				if comment.Author != nil {
					author = comment.Author.Login
				}
				if i == 0 {
					thread.Body = comment.Body
					if comment.Author != nil {
						thread.Author = comment.Author.Login
						thread.Bot = comment.Author.Typename == "Bot" || strings.HasSuffix(comment.Author.Login, "[bot]")
					}
					continue
				}
				thread.Replies = append(thread.Replies, ReviewConversationComment{Author: author, Body: comment.Body})
				for _, match := range landingReviewThreadSurfaced.FindAllStringSubmatch(comment.Body, -1) {
					thread.Surfaced = append(thread.Surfaced, match[1])
				}
			}
			threads = append(threads, thread)
		}
		if !connection.PageInfo.HasNextPage {
			return threads, nil
		}
		if connection.PageInfo.EndCursor == "" || connection.PageInfo.EndCursor == after {
			return nil, errors.New("the review thread connection did not advance its pagination cursor")
		}
		after = connection.PageInfo.EndCursor
	}
	return nil, errors.New("the review thread connection exceeded the pagination limit")
}

func replyLandingReviewThread(ctx context.Context, client GitHubRESTClient, thread, body string) error {
	return client.GraphQL(ctx, landingReviewThreadReply, map[string]any{"thread": thread, "body": body}, &struct{}{})
}

func landingReviewThreadsReason(threads []ReviewConversation) string {
	var b strings.Builder
	b.WriteString(ReviewConversationsRefusalText)
	b.WriteString(" Address each finding in Rework, then resolve its conversation with evidence of the fix or dismissal.")
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

func ReadReviewConversations(ctx context.Context, client GitHubRESTClient, opts LandOptions) ([]ReviewConversation, error) {
	repository, _, ok := githubLandingRepository(opts.Repository)
	if !ok || opts.External == nil || opts.External.Provider != "github" || opts.External.URL != opts.Repository+"/pull/"+opts.External.ID || opts.HeadSHA == "" {
		return nil, errors.New("review conversation source is unavailable")
	}
	number, err := strconv.Atoi(opts.External.ID)
	if err != nil || number <= 0 || strconv.Itoa(number) != opts.External.ID || client == nil {
		return nil, errors.New("review conversation identity is unavailable")
	}
	return readLandingReviewThreads(ctx, client, repository, number, &opts)
}

func ResolveReviewConversation(ctx context.Context, client GitHubRESTClient, opts LandOptions, threadID, disposition, evidence string) error {
	if strings.TrimSpace(threadID) == "" || disposition != "fixed" && disposition != "dismissed" || strings.TrimSpace(evidence) == "" || len(evidence) > 8192 || opts.Authorize == nil {
		return errors.New("review disposition requires a thread, fixed or dismissed decision, evidence and current source authority")
	}
	check := func() error {
		if err := opts.Authorize(ctx); err != nil {
			return err
		}
		threads, err := ReadReviewConversations(ctx, client, opts)
		if err != nil {
			return err
		}
		for _, thread := range threads {
			if thread.ID != threadID {
				continue
			}
			if !thread.Bot {
				return errors.New("a human-authored conversation requires its existing human review owner")
			}
			if thread.MoreComments {
				return errors.New("remaining review replies are unavailable; cannot resolve an incomplete conversation")
			}
			return nil
		}
		return errors.New("the unresolved conversation is not part of the reviewed pull request")
	}
	if err := check(); err != nil {
		return err
	}
	body := "Detent disposition at published head " + opts.HeadSHA + ": " + disposition + ".\n\n" + strings.TrimSpace(evidence)
	if err := replyLandingReviewThread(ctx, client, threadID, body); err != nil {
		return fmt.Errorf("record review disposition: %w", err)
	}
	if err := check(); err != nil {
		return err
	}
	var result struct {
		Resolve struct {
			Thread struct {
				ID       string `json:"id"`
				Resolved bool   `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	if err := client.GraphQL(ctx, landingReviewThreadResolve, map[string]any{"thread": threadID}, &result); err != nil {
		return fmt.Errorf("resolve reviewed conversation: %w", err)
	}
	if result.Resolve.Thread.ID != threadID || !result.Resolve.Thread.Resolved {
		return errors.New("GitHub did not acknowledge the resolved conversation")
	}
	if err := opts.Authorize(ctx); err != nil {
		return err
	}
	_, err := ReadReviewConversations(ctx, client, opts)
	return err
}

func shortLandingHead(head string) string {
	if len(head) > 12 {
		return head[:12]
	}
	return head
}
