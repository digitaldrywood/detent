package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type auditEvidenceResult struct {
	RunID              int64                       `json:"run_id"`
	InvocationID       string                      `json:"invocation_id"`
	ProjectID          string                      `json:"project_id"`
	Repository         string                      `json:"repository"`
	PullRequest        int                         `json:"pull_request"`
	BaseSHA            string                      `json:"base_sha"`
	HeadSHA            string                      `json:"head_sha"`
	ServiceIdentity    string                      `json:"service_identity"`
	ReviewerVersion    string                      `json:"reviewer_version"`
	ReviewerDigest     string                      `json:"reviewer_digest"`
	AuthenticationMode string                      `json:"authentication_mode"`
	ExitStatus         string                      `json:"exit_status"`
	FailureReason      string                      `json:"failure_reason,omitempty"`
	ActualBytes        int                         `json:"actual_bytes,omitempty"`
	MaxDiffBytes       int                         `json:"max_diff_bytes,omitempty"`
	OutputDigest       string                      `json:"output_digest"`
	OutputBytes        int                         `json:"output_bytes"`
	Verdict            string                      `json:"verdict"`
	Summary            string                      `json:"summary"`
	Findings           []securityaudit.Finding     `json:"findings"`
	Dispositions       []securityaudit.Disposition `json:"dispositions"`
	Attempt            int                         `json:"attempt"`
	StartedAt          string                      `json:"started_at"`
	CompletedAt        string                      `json:"completed_at"`
	RecordedAt         string                      `json:"recorded_at"`
}

func newAuditCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "audit",
		Short:   "Inspect and disposition trusted security audits",
		Example: "detent audit evidence --project example-project --repository example-org/example-project --pull-request 2006",
	}
	cmd.AddCommand(
		newAuditEvidenceCommand(configPath, opts),
		newAuditDispositionCommand(configPath, host, port, opts),
	)
	return cmd
}

func newAuditDispositionCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var projectID string
	var repository string
	var pullRequest int
	var baseSHA string
	var headSHA string
	var findingID string
	var evidence string
	var confirmed bool
	cmd := &cobra.Command{
		Use:     "disposition",
		Short:   "Record an evidenced false-positive disposition through the trusted Detent service",
		Example: "detent audit disposition --project example-project --repository example-org/example-project --pull-request 2006 --base-sha abc --head-sha def --finding auth-1 --evidence 'Endpoint requires an authenticated administrator before this path is reachable.' --yes",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !confirmed {
				return WrapValidation(errors.New("security audit disposition requires --yes confirmation"))
			}
			result, err := runAuditDisposition(cmd.Context(), derefString(configPath), derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), projectID, repository, pullRequest, baseSHA, headSHA, findingID, evidence, opts)
			if err != nil {
				return err
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			return out.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintf(writer, "Recorded %s disposition for finding %s on audit run %d at %s\n", result.Status, result.FindingID, result.AuditRunID, result.RecordedAt.UTC().Format(time.RFC3339Nano))
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "project id")
	cmd.Flags().StringVar(&repository, "repository", "", "pull request repository")
	cmd.Flags().IntVar(&pullRequest, "pull-request", 0, "pull request number")
	cmd.Flags().StringVar(&baseSHA, "base-sha", "", "exact base commit")
	cmd.Flags().StringVar(&headSHA, "head-sha", "", "exact head commit")
	cmd.Flags().StringVar(&findingID, "finding", "", "audit finding id")
	cmd.Flags().StringVar(&evidence, "evidence", "", "evidence supporting the false-positive disposition")
	cmd.Flags().BoolVar(&confirmed, "yes", false, "confirm the false-positive disposition")
	for _, flag := range []string{"project", "repository", "pull-request", "base-sha", "head-sha", "finding", "evidence"} {
		markFlagRequired(cmd, flag)
	}
	return cmd
}

func runAuditDisposition(
	ctx context.Context,
	configPath string,
	host string,
	port int,
	portSet bool,
	projectID string,
	repository string,
	pullRequest int,
	baseSHA string,
	headSHA string,
	findingID string,
	evidence string,
	opts options,
) (securityaudit.Disposition, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return securityaudit.Disposition{}, err
	}
	global, err := opts.read(resolution.Path)
	if err != nil {
		return securityaudit.Disposition{}, err
	}
	if global.Client.Configured() {
		clientConfig := global.Client.Normalized()
		project, ok := clientConfig.NativeProjects[projectID]
		if !ok {
			return securityaudit.Disposition{}, errors.New("project is not configured for the Hub runner")
		}
		client, err := api.New(api.Config{URL: clientConfig.URL, IdentityFile: clientConfig.IdentityFile, TokenSource: func() string { return opts.lookupEnv(clientConfig.TokenEnvironment) }, HTTPClient: &http.Client{Transport: auditTransport{do: opts.httpDo}, Timeout: clientConfig.RequestTimeout()}})
		if err != nil {
			return securityaudit.Disposition{}, err
		}
		native, err := client.Native(tracker.OrganizationID(clientConfig.OrganizationID), tracker.ProjectID(project))
		if err != nil {
			return securityaudit.Disposition{}, err
		}
		return native.RecordAuditDisposition(ctx, securityaudit.DispositionCommand{Key: securityaudit.Key{ProjectID: project, Repository: repository, PRNumber: pullRequest, BaseSHA: baseSHA, HeadSHA: headSHA}, FindingID: findingID, Evidence: evidence, Confirm: true})
	}
	return securityaudit.Disposition{}, errors.New("security audit commands require configured Hub access")
}

type auditTransport struct {
	do func(*http.Request) (*http.Response, error)
}

func (t auditTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.do(request)
}

func newAuditEvidenceCommand(configPath *string, opts options) *cobra.Command {
	var projectID string
	var repository string
	var pullRequest int
	var baseSHA string
	var headSHA string
	cmd := &cobra.Command{
		Use:     "evidence",
		Short:   "Show immutable audit evidence for a pull request",
		Example: "detent audit evidence --project example-project --repository example-org/example-project --pull-request 2006",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (strings.TrimSpace(baseSHA) == "") != (strings.TrimSpace(headSHA) == "") {
				return WrapValidation(errors.New("--base-sha and --head-sha must be provided together"))
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			resolution, err := resolveConfigPathResolution(*configPath, opts)
			if err != nil {
				return err
			}
			evidence, err := loadAuditEvidence(cmd.Context(), resolution.Path, securityaudit.Key{ProjectID: projectID, Repository: repository, PRNumber: pullRequest, BaseSHA: baseSHA, HeadSHA: headSHA}, opts)
			if err != nil {
				return err
			}
			run, dispositions := evidence.Run, evidence.Dispositions
			result := auditEvidenceResultFromRun(run, dispositions)
			return out.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintf(writer, "Audit run %d (%s)\nPR: %s#%d\nBase: %s\nHead: %s\nService: %s\nReviewer: %s (%s)\nAuthentication: %s\nExit: %s\nOutput: %s (%d bytes)\nVerdict: %s\nSummary: %s\n", result.RunID, result.InvocationID, result.Repository, result.PullRequest, result.BaseSHA, result.HeadSHA, result.ServiceIdentity, result.ReviewerVersion, result.ReviewerDigest, result.AuthenticationMode, result.ExitStatus, result.OutputDigest, result.OutputBytes, result.Verdict, result.Summary)
				if err == nil && result.FailureReason == securityaudit.ReasonDiffTooLarge {
					_, err = fmt.Fprintf(writer, "Failure: %s (actual %d bytes; security_audit.max_diff_bytes %d bytes)\n", result.FailureReason, result.ActualBytes, result.MaxDiffBytes)
				}
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "project id")
	cmd.Flags().StringVar(&repository, "repository", "", "pull request repository")
	cmd.Flags().IntVar(&pullRequest, "pull-request", 0, "pull request number")
	cmd.Flags().StringVar(&baseSHA, "base-sha", "", "exact base commit")
	cmd.Flags().StringVar(&headSHA, "head-sha", "", "exact head commit")
	markFlagRequired(cmd, "project")
	markFlagRequired(cmd, "repository")
	markFlagRequired(cmd, "pull-request")
	return cmd
}

func auditEvidenceResultFromRun(run securityaudit.Run, dispositions []securityaudit.Disposition) auditEvidenceResult {
	result := auditEvidenceResult{
		RunID:              run.ID,
		InvocationID:       run.InvocationID,
		ProjectID:          run.ProjectID,
		Repository:         run.Repository,
		PullRequest:        run.PRNumber,
		BaseSHA:            run.BaseSHA,
		HeadSHA:            run.HeadSHA,
		ServiceIdentity:    run.ServiceIdentity,
		ReviewerVersion:    run.ReviewerVersion,
		ReviewerDigest:     run.ReviewerDigest,
		AuthenticationMode: run.AuthenticationMode,
		ExitStatus:         run.ExitStatus,
		OutputDigest:       run.OutputDigest,
		OutputBytes:        run.OutputBytes,
		Verdict:            run.Verdict,
		Summary:            run.Summary,
		Findings:           append([]securityaudit.Finding(nil), run.Findings...),
		Dispositions:       append([]securityaudit.Disposition(nil), dispositions...),
		Attempt:            run.Attempt,
		StartedAt:          run.StartedAt.UTC().Format(time.RFC3339Nano),
		CompletedAt:        run.CompletedAt.UTC().Format(time.RFC3339Nano),
		RecordedAt:         run.RecordedAt.UTC().Format(time.RFC3339Nano),
	}
	if size, ok := securityaudit.ParseDiffTooLargeFailure(run.Failure); ok && run.ExitStatus == securityaudit.ExitStatusFailed {
		result.FailureReason = securityaudit.ReasonDiffTooLarge
		result.ActualBytes = size.ActualBytes
		result.MaxDiffBytes = size.LimitBytes
	}
	return result
}

func loadAuditEvidence(ctx context.Context, path string, key securityaudit.Key, opts options) (securityaudit.Evidence, error) {
	cfg, err := opts.read(path)
	if err != nil {
		return securityaudit.Evidence{}, err
	}
	if cfg.Client.Configured() {
		normalized := cfg.Client.Normalized()
		project, ok := normalized.NativeProjects[key.ProjectID]
		if !ok {
			return securityaudit.Evidence{}, errors.New("project is not configured for the Hub runner")
		}
		client, err := api.New(api.Config{URL: normalized.URL, IdentityFile: normalized.IdentityFile, TokenSource: func() string { return opts.lookupEnv(normalized.TokenEnvironment) }, HTTPClient: &http.Client{Transport: auditTransport{do: opts.httpDo}, Timeout: normalized.RequestTimeout()}})
		if err != nil {
			return securityaudit.Evidence{}, err
		}
		native, err := client.Native(tracker.OrganizationID(normalized.OrganizationID), tracker.ProjectID(project))
		if err != nil {
			return securityaudit.Evidence{}, err
		}
		return native.AuditEvidence(ctx, key)
	}
	return securityaudit.Evidence{}, errors.New("security audit commands require configured Hub access")
}
