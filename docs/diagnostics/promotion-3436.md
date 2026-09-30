# Promotion read profile — issue #3436

Measured on 2026-09-30 against signed v0.117.7 source `a53a7f204` and the
#3436 patch applied to that source. These are controlled calls to
`autoPromoteHumanReviewIssues` for one Merging issue with a passing, trusted
exact-head/base audit. Setup supplies a fully hydrated PR snapshot as the tick
fetch does; setup requests are excluded. Cold means a new measured connector;
warm repeats with that connector and its existing branch-policy cache.

The HTTP client counts each outgoing request by REST path or GraphQL operation,
including conditional requests. It rejects GraphQL mutations. The harness uses
an in-memory audit store and does not touch either live instance, database,
tracker lanes, or PR contents. Both before/after calls leave Merging unchanged.

## Comparable samples

| Host/project | PR head (both versions) | Released cold / warm | Patched cold / warm | Released REST + GraphQL cold / warm | Patched requests |
|---|---|---|---|---|---|
| Mac / Detent | `c6eb99858d9bc6d99441cc5c297323e10c2fa99c` (#3435) | 2.267041s / 1.979825s | 208.750µs / 14.250µs | 7 + 1 / 5 + 1 | 0 / 0 |
| Prometheus / Pyro | `50be9811da3f53fcbf4084412b3386db83d81247` (#3232) | 2.218932s / 1.628063s | 119.461µs / 20.428µs | 7 + 1 / 5 + 1 | 0 / 0 |

An earlier Mac sample used head `44d367cc86a9d96650ded69e3cc78da189bf4e30`:
2.191672s / 1.604954s and identical request counts. The table uses the repeated
released sample matching the patched head, after the live PR head advanced.
This is one cold/warm pair per version, not a statistical latency distribution.
Microsecond timings reflect the in-memory audit fixture, not production SQLite.

## Per-operation requests

Paths below use each project's repository, PR number, current head, and
`develop` target. Counts were identical on both hosts.

| Operation | Released cold | Released warm | Patched cold | Patched warm |
|---|---:|---:|---:|---:|
| REST `GET /pulls/<PR>` | 1 | 1 | 0 | 0 |
| REST `GET /commits/<head>/check-runs` | 1 | 1 | 0 | 0 |
| REST `GET /commits/<head>/statuses` | 1 | 1 | 0 | 0 |
| REST `GET /pulls/<PR>/reviews` | 1 | 1 | 0 | 0 |
| REST `GET /issues/<PR>/comments` (automated review summary) | 1 | 1 | 0 | 0 |
| REST `GET /rules/branches/develop` | 1 | 0 | 0 | 0 |
| REST `GET /branches/develop/protection/required_status_checks` | 1 | 0 | 0 | 0 |
| GraphQL `DetentGitHubPullRequestReviewThreads` | 1 | 1 | 0 | 0 |

The repeated full hydration dominated this isolated promotion call; no request
saving is inferred from the aggregate refresh timings. Full hydration overlaps
the tick's PR metadata/check/status/review hydration. Its review-thread request
and full gate evaluation are inapplicable to Merging's audit-only promotion
result. Passing/running audits now consume the supplied head/base identity.
A lane-changing failure still refreshes live PR identity before Rework, including
when a newer head supersedes the finding or an earlier transition already won.
Degraded/unavailable snapshots cannot trigger that transition. Missing audit
collection continues through the existing audit stage and its identity checks.

## Runtime evidence and limits

Historical Mac refresh `dlsthrh7fccw-7` recorded promotion from
17:14:49.079788Z to 17:15:43.280767Z (54.200837s). Between those boundaries the
shared credential's debug log contains 11 review-thread GraphQL requests, two
association-reference requests, six coordination-state queries, three coordination
commits, four branch-PR queries, and three candidate-hydration queries. Those
interleaved operations are not all attributable to one promotion call. REST
request logging was disabled, so this historical trace does not establish total
REST counts or savings. Prometheus refresh `dlsti2hwago0-84` recorded promotion
from 17:18:10.871300Z to 17:20:00.079677Z (109.208380s).

No repaired release was deployed or live instance restarted. These measurements
show the removed per-Merging read work; they do not claim a new end-to-end
refresh duration or dispatch throughput. Human Review/Rework still require live
reads, and a slow applicable read can still occupy the actor. Existing background
workers continue, with the existing runtime overlay published at tick entry and
fresh heartbeat pointers exposed while reads are pending.

## Focused regressions

Before the patch, `TestMergingPromotionUsesFetchedAuditIdentity` failed with one
PR hydration plus one thread hydration per case. It now covers passing, pending
and red producers, drafts, threads, changed head/base, audit findings, findings
superseded by a live head, and unavailable/degraded snapshots. It asserts no
merge occurs in promotion. Existing merge-worker/gate tests retain the live
checks, strict/native policy, authorization, operational completion, and holds.

`TestPromotionReadKeepsRecoveredWorkersObservable` replays nine active attempts
behind an older empty snapshot with a blocked promotion hydrator. Before the
patch it returned zero workers/attempts; after the patch it returns all nine and
fresh worker/attempt messages during both progress updates. It uses synctest
and does not sleep in wall time.

Focused promotion, audit, merge-worker, required-check, and snapshot diagnostics
passed in the orchestrator and GitHub connector packages. Orchestrator vet
passed. No full suite, race/coverage gate, or per-PR CI gate ran.

## Reproduction harness

Extract released source with `git archive a53a7f204` into the worker's provided
TMPDIR. Add the following diagnostic test under `internal/orchestrator/`, then
run with `PROFILE_REPO=digitaldrywood/detent PROFILE_PR=3435 go test -p 4
./internal/orchestrator -run '^TestPromotionReadProfile$' -count=1 -v`.
For the patch comparison, replace only the two changed production files and
repeat. PR heads must match in the recorded output for a comparable pair.

Prometheus ran the same source as a Linux/amd64 test binary with
`PROFILE_REPO=digitaldrywood/pyroapex PROFILE_PR=3232`; the binary was streamed
through SSH into a Linux memfd and executed without remote scratch files.
All Mac scratch and captured output stayed in the provided TMPDIR. The two
preliminary diagnostic runs caught an unclosed harness transport; closing the
setup connector fixed the harness before the reported successful samples.

```go
package orchestrator
import("bytes";"context";"encoding/json";"fmt";"io";"net/http";"os";"os/exec";"strconv";"strings";"sync";"testing";"time"
"github.com/digitaldrywood/detent/internal/connector";"github.com/digitaldrywood/detent/internal/connector/github")
type promotionProfileHTTP struct { mu sync.Mutex; counts map[string]int; durations map[string]time.Duration }
func(p *promotionProfileHTTP) Do(r *http.Request)(*http.Response,error){
 key:=r.Method+" "+r.URL.Path
 if r.Method!="GET" { body,_:=io.ReadAll(r.Body); r.Body=io.NopCloser(bytes.NewReader(body)); var q struct{Query string};json.Unmarshal(body,&q);if strings.Contains(q.Query,"mutation "){return nil,fmt.Errorf("read-only profile refuses mutation")};f:=strings.Fields(q.Query);if len(f)>1{key="GraphQL "+strings.Split(f[1],"(")[0]} }
 start:=time.Now();res,err:=http.DefaultClient.Do(r);p.mu.Lock();p.counts[key]++;p.durations[key]+=time.Since(start);p.mu.Unlock();return res,err
}
func TestPromotionReadProfile(t *testing.T){
 defer http.DefaultClient.CloseIdleConnections()
 repo:=os.Getenv("PROFILE_REPO");n,_:=strconv.Atoi(os.Getenv("PROFILE_PR"));if repo==""||n==0{t.Skip("explicit read-only profile")}
 token,err:=exec.Command("gh","auth","token").Output();if err!=nil{t.Fatal(err)}
 p:=&promotionProfileHTTP{};tracker,err:=github.NewConnector(github.Config{APIKey:strings.TrimSpace(string(token)),Repository:repo,GitHubStatusSource:"label",RequiredStatusChecks:[]string{},HTTPClient:p});if err!=nil{t.Fatal(err)}
 issue:=securityAuditTestIssue();issue.PRRepository=repo;issue.PRNumber=&n;issue.PullRequest.Number=n;issue.State="Merging"
 // Setup supplies the same fully hydrated current-head snapshot the tick fetch owns.
 setup,err:=github.NewConnector(github.Config{APIKey:strings.TrimSpace(string(token)),Repository:repo,GitHubStatusSource:"label",RequiredStatusChecks:[]string{}});if err!=nil{t.Fatal(err)}
 defer setup.Close()
 issue,err=setup.HydratePullRequest(context.Background(),issue);if err!=nil{t.Fatal(err)}
 memo:=newSecurityAuditMemoryStore();_,err=memo.RecordSecurityAuditRun(context.Background(),securityAuditPassingRun(issue));if err!=nil{t.Fatal(err)}
 o:=securityAuditTestOrchestrator(memo);o.cfg.AutoPromote.Enabled=true;o.cfg.ActiveStates=[]string{"Todo","In Progress","Rework","Merging"};o.cfg=normalizeConfig(o.cfg);o.connector=tracker;state:=newState(o.cfg)
 for _,phase:=range []string{"cold","warm"}{
 p.counts=map[string]int{};p.durations=map[string]time.Duration{}
 start:=time.Now();result:=o.autoPromoteHumanReviewIssues(context.Background(),&state,[]connector.Issue{issue},time.Now())
 if len(result.transitioned)>0{t.Fatal("passing Merging audit changed lane")}
 t.Logf("PROFILE phase=%s repo=%s pr=%d head=%s duration=%s requests=%v operation_time=%v",phase,repo,n,issue.PullRequest.HeadSHA,time.Since(start),p.counts,p.durations)
 }
}
```
