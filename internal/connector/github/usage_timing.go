package github

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func (c *Client) resolveRequestToken(ctx context.Context, attribution connector.RESTScopeAttribution) (string, error) {
	started := time.Now()
	token, err := c.tokenSource.Token(ctx)
	outcome := "200"
	if err != nil || strings.TrimSpace(token) == "" {
		outcome = "error"
	}
	attribution.Observe("token_resolution_inclusive", outcome, time.Since(started))
	return token, err
}

type httpAttemptTiming struct {
	attribution connector.RESTScopeAttribution
	response    *http.Response
	started     time.Time
	elapsed     time.Duration
	outcome     string
	drain       bool
}

func timedHTTPAttempt(attribution connector.RESTScopeAttribution, client HTTPClient, req *http.Request, rest bool, drain bool) (*http.Response, error, *httpAttemptTiming) {
	started := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(started)
	outcome := classifyRESTScopeOutcome(resp, err)
	if rest {
		attribution.Record(outcome)
	}
	if err != nil {
		attribution.Observe("http_transport", outcome, elapsed)
		return resp, err, nil
	}
	return resp, nil, &httpAttemptTiming{attribution: attribution, response: resp, elapsed: elapsed, outcome: outcome, drain: drain}
}

func (t *httpAttemptTiming) BeginBody() {
	t.started = time.Now()
}

func (t *httpAttemptTiming) BodyConsumed(err error) {
	t.elapsed += time.Since(t.started)
	if err != nil {
		t.outcome = "error"
	}
}

func (t *httpAttemptTiming) Close() error {
	started := time.Now()
	var err error
	if t.drain {
		err = drainAndClose(t.response.Body)
	} else {
		err = t.response.Body.Close()
	}
	t.elapsed += time.Since(started)
	if err != nil {
		t.outcome = "error"
	}
	t.attribution.Observe("http_transport", t.outcome, t.elapsed)
	return err
}

func fixedGraphQLTimingPurpose(queryType string) string {
	switch queryType {
	case graphQLQueryAuthenticate, graphQLQueryCandidateIssues, graphQLQueryObservedStatus,
		graphQLQueryRunningStates, graphQLQueryEpicChildren, graphQLQueryIssueParents,
		graphQLQueryPullRequests, graphQLQueryReviewThreads, graphQLQueryBlockedReasons,
		graphQLQueryIssueLookup, graphQLQueryProjectItem, graphQLQueryProjectMetadata,
		graphQLQueryAssignees, graphQLQueryCreateComment, graphQLQueryCloseIssue,
		graphQLQuerySetAssignee, graphQLQueryRemoveAssignees, graphQLQueryUpdateField,
		graphQLQueryRemoveItem, graphQLQueryAddProjectItem, graphQLQueryMergeQueue,
		graphQLQueryEnqueuePR, graphQLQueryDequeuePR, graphQLQueryRateLimitProbe,
		graphQLQueryLaneSignalStatus, "onboarding_issue_discovery", "linked_issue_intake":
		return queryType
	default:
		return "graphql"
	}
}
