package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseAuditReporter struct{ destination *cloudDestination }

func (r releaseAuditReporter) Confirmed(ctx context.Context, identity releaseaudit.Identity, key string, record json.RawMessage) error {
	var finding struct {
		Verdict     string `json:"verdict"`
		Title       string `json:"title"`
		Fingerprint string `json:"fingerprint"`
	}
	if json.Unmarshal(record, &finding) != nil || finding.Verdict != "confirmed" || key == "" || finding.Fingerprint == "" {
		return errors.New("release audit repair requires a confirmed source-derived record")
	}
	record, err := json.Marshal(record)
	if err != nil {
		return errors.New("release audit repair record is invalid")
	}
	body := fmt.Sprintf("Release security audit at pinned commit `%s`.\n\nRun: %s\nJob: %s\nRoot cause key: `%s`\nSource fingerprint: `%s`\nTrusted skill commit: `%s`\nTrusted skill digest: `%s`\n\nConfirmed record (untrusted audit data):\n\n```json\n%s\n```\n\nRepair this reproduced source boundary; preserve existing work and human holds. This stage reports the issue and does not wait for its repair.\n\n```detent-agent\nschema: 1\neffort: high\n```", identity.Commit, identity.RunURL, identity.JobID, key, html.EscapeString(finding.Fingerprint), releaseaudit.BundleCommit, releaseaudit.BundleDigest, record)
	return r.file(ctx, identity, key, finding.Title, body, false)
}

func (r releaseAuditReporter) Infrastructure(ctx context.Context, identity releaseaudit.Identity, diagnostic string) error {
	key := issueorigin.Fingerprint("release-audit-infrastructure:" + identity.ProjectID)
	body := fmt.Sprintf("Release security audit infrastructure failure at pinned commit `%s`.\n\nRun: %s\nJob: %s\n\n%s\n\nThis is an instance failure, not a vulnerability finding. Source repair requires a reproduced source or test diagnostic.\n\n```detent-agent\nschema: 1\neffort: high\n```", identity.Commit, identity.RunURL, identity.JobID, diagnostic)
	return r.file(ctx, identity, key, "release security audit infrastructure failure", body, true)
}

func (r releaseAuditReporter) file(ctx context.Context, identity releaseaudit.Identity, key, summary, body string, infrastructure bool) error {
	if identity.ProjectID != r.destination.project || !stabilizationSeries.MatchString(identity.ReleaseSeries) {
		return errors.New("release audit reporting requires the selected native release-series owner")
	}
	fingerprint := issueorigin.ReleaseStabilizationFingerprint(identity.ReleaseSeries)
	body += "\n\n" + stabilizationMarker(identity.ReleaseSeries) + "\n\nProblem: `" + key + "`"
	body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: identity.RunURL, Fingerprint: fingerprint})
	occurrence := issueorigin.Fingerprint(strings.Join([]string{identity.ProjectID, identity.RunID, identity.Attempt, identity.JobID, key}, ":"))
	labels := []string{"ci-scheduled-failure"}
	if infrastructure {
		labels = append(labels, "ci-infrastructure-failure")
	}
	return r.destination.file(ctx, fingerprint, identity.ReleaseSeries+" stabilization: "+summary, body, occurrence, labels, false)
}

func runReleaseAudit(ctx context.Context, input io.Reader, getenv func(string) string, output io.Writer) error {
	var request struct {
		Identity   releaseaudit.Identity  `json:"identity"`
		Allowance  releaseaudit.Allowance `json:"allowance"`
		Repository string                 `json:"repository_directory"`
		Model      string                 `json:"model"`
	}
	decoder := json.NewDecoder(io.LimitReader(input, 32769))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return errors.New("release audit request is invalid")
	}
	if request.Identity.ProjectID != getenv("DETENT_PROJECT_ID") || request.Identity.Repository != getenv("GITHUB_REPOSITORY") || request.Identity.Commit != getenv("CI_DEVELOP_SHA") || request.Identity.RunID != getenv("GITHUB_RUN_ID") || request.Identity.Attempt != getenv("GITHUB_RUN_ATTEMPT") || request.Identity.ReleaseSeries != getenv("CI_RELEASE_BRANCH") || !stabilizationSeries.MatchString(request.Identity.ReleaseSeries) || request.Model == "" {
		return errors.New("release audit request does not match the selected pinned scheduled release")
	}
	scratch := getenv("TMPDIR")
	if scratch == "" {
		scratch = getenv("TMP")
	}
	if scratch == "" {
		scratch = getenv("TEMP")
	}
	if !filepath.IsAbs(scratch) {
		return errors.New("release audit requires job-owned scratch")
	}
	token := getenv("DETENT_API_KEY")
	if token == "" {
		return errors.New("release audit requires its scoped native reporting credential")
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	transport, err := newCloudREST(getenv("DETENT_MCP_URL"), request.Identity.ProjectID, token, httpClient)
	if err != nil {
		return err
	}
	destination := &cloudDestination{command: transport.call, project: request.Identity.ProjectID, evidence: output}
	if err := destination.load(ctx); err != nil {
		return err
	}
	endpoint, err := url.Parse(transport.base)
	if err != nil {
		return err
	}
	endpoint.Path = ""
	client, err := hubclient.New(hubclient.Config{URL: endpoint.String(), TokenSource: func() string { return token }, HTTPClient: httpClient})
	if err != nil {
		return err
	}
	native, err := client.Native(tracker.OrganizationID(destination.organization), tracker.ProjectID(destination.project))
	if err != nil {
		return err
	}
	source, err := releaseaudit.NewGitSource(ctx, request.Repository, request.Identity.Commit, scratch)
	if err != nil {
		return err
	}
	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		command := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://")
		command.Dir = scratch
		return command
	})
	if err != nil {
		return err
	}
	server, err := codex.NewAppServer(factory)
	if err != nil {
		return err
	}
	provider, err := codex.NewReleaseAuditProvider(server, request.Model)
	if err != nil {
		return err
	}
	sandbox, sandboxErr := source.Sandbox(ctx, scratch)
	if sandboxErr != nil {
		if _, err := fmt.Fprintln(output, "Target execution sandbox is unavailable; execution-dependent leads remain needs_validation."); err != nil {
			return err
		}
	}
	stage, err := releaseaudit.New(provider, native.ReleaseAuditRecords(), releaseAuditReporter{destination: destination}, sandbox)
	if err != nil {
		return err
	}
	result, err := stage.Run(ctx, releaseaudit.Request{Identity: request.Identity, Allowance: request.Allowance, Source: source, Scratch: scratch})
	encodeErr := json.NewEncoder(output).Encode(result)
	if err == nil && result.Status == "incomplete" {
		err = errors.New("release audit is incomplete; retained state is not a clean audit")
	}
	return errors.Join(err, encodeErr)
}
