package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/stoptest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestStoppedPublicationReconciliation(t *testing.T) {
	for _, effect := range []string{"git_push", "pr_create"} {
		for _, state := range []string{"confirmed", "ambiguous"} {
			for _, delivery := range []string{"accepted", "undelivered"} {
				for _, policy := range []string{"stale", "current"} {
					t.Run(effect+"/"+state+"/"+delivery+"/"+policy, func(t *testing.T) {
						runStoppedPublication(t, effect, state, delivery, policy)
					})
				}
			}
		}
	}
}

func runStoppedPublication(t *testing.T, effect, state, delivery, policy string) {
	t.Helper()
	h := newNativeChangeHub(t, true)
	h.descriptor.Gates.GitHubPullRequest = true
	h.repolicy(t)
	issue := h.createInProgress(t, "Stop after publication")
	h.claim(t, issue.ID)
	e := h.scheduler.RunExecution(issue.ID).(fixtureLifecycle)
	head := strings.Repeat("c", 40)
	transport := fixtureClient(h.native.Transport()).requests
	worker, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	ctx, stop, err := e.Guard(worker)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := e.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	e.SetDiffSource(nativeChangeDiff(head, "README.md"))
	e.SetRepository(nativeChangeRepository)
	if err := e.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
		t.Fatal(err)
	}
	next := transport.Load()
	var pending, candidate *tracker.NativeCheckpoint
	var seq int64
	var posts int
	transport.Store(testsupport.RoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events") {
			event := stoptest.ReadEvent(t, r)
			checkpoint := event.Data.Handoff
			if event.Type == "run.checkpointed" && checkpoint != nil && checkpoint.ExternalEffect == effect {
				switch checkpoint.EffectState {
				case "pending":
					pending = checkpoint
				case state:
					posts++
					if posts == 1 {
						candidate, seq = checkpoint, event.Data.Sequence
						if delivery == "accepted" {
							resp, err := next.RoundTrip(r)
							if err != nil {
								return nil, err
							}
							resp.Body.Close()
							if resp.StatusCode != http.StatusOK {
								t.Fatalf("receipt response %d", resp.StatusCode)
							}
						}
						return nil, io.ErrUnexpectedEOF
					}
				}
			}
		}
		res, err := next.RoundTrip(r)
		if err != nil || candidate == nil || policy != "stale" || r.Method != http.MethodGet || r.URL.Query().Get("view") != "bounded" || !strings.Contains(r.URL.Path, "/changes/") {
			return res, err
		}
		var page operatortool.ChangeResult
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
			return nil, err
		}
		page.Detail.Summary.Status = "stale_policy"
		body, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		res.Body = io.NopCloser(bytes.NewReader(body))
		res.ContentLength = int64(len(body))
		return res, nil
	}))
	calls := 0
	e.SetPublicationSource(func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
		calls++
		p := workspace.GitHubPublication{Repository: version.Repository, HeadSHA: version.HeadSHA, Branch: "native", BaseRef: "main"}
		for _, kind := range []string{"git_push", "pr_create"} {
			if err := opts.PublicationEffect(ctx, kind, "pending", p); err != nil {
				return p, err
			}
			if kind == "pr_create" {
				p.External = tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}
			}
			if kind == effect {
				cancel(runner.ErrOperatorStopped)
				return p, opts.PublicationEffect(ctx, kind, state, p)
			}
			if err := opts.PublicationEffect(ctx, kind, "confirmed", p); err != nil {
				return p, err
			}
		}
		return p, nil
	})
	if err := e.PrepareFinish(context.WithoutCancel(ctx), "succeeded", "", nil); err == nil || candidate == nil || pending == nil || posts != 1 {
		t.Fatalf("publication setup: pending %+v, receipt %+v, posts %d, error %v", pending, candidate, posts, err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, runner.FinalStateOperatorStopped); err != nil {
		t.Fatalf("stop after %s receipt: %v", delivery, err)
	}
	want, wantSeq, wantPosts := candidate, seq+1, 1
	if delivery == "undelivered" {
		if policy == "stale" {
			want, wantSeq = pending, seq
		} else {
			wantPosts++
		}
	}
	attempt, err := h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), e.CompletionData().AttemptID)
	if err != nil || calls != 1 || posts != wantPosts || attempt.ClaimReleasedAt == nil || attempt.Outcome != "interrupted" || attempt.Sequence != wantSeq || !reflect.DeepEqual(attempt.Checkpoint, want) {
		t.Fatalf("calls %d, receipt POSTs %d want %d, released %v, outcome %q, sequence %d want %d, checkpoint %+v want %+v, error %v", calls, posts, wantPosts, attempt.ClaimReleasedAt, attempt.Outcome, attempt.Sequence, wantSeq, attempt.Checkpoint, want, err)
	}
}
