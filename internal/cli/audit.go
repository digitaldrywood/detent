package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
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

func newAuditCommand(configPath *string, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "audit",
		Short:   "Inspect trusted security audit evidence",
		Example: "detent audit evidence --project detent --repository digitaldrywood/detent --pull-request 2006",
	}
	cmd.AddCommand(newAuditEvidenceCommand(configPath, opts))
	return cmd
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
		Example: "detent audit evidence --project detent --repository digitaldrywood/detent --pull-request 2006",
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
			backend, err := store.Open(cmd.Context(), store.Config{Backend: store.BackendSQLite, Path: doctorRuntimeStorePath(resolution.Path)})
			if err != nil {
				return err
			}
			defer func() {
				if err := backend.Close(); err != nil {
					slog.Warn("close audit evidence store", "error", err)
				}
			}()
			var run securityaudit.Run
			if strings.TrimSpace(headSHA) != "" {
				run, err = backend.LatestSecurityAuditRun(cmd.Context(), securityaudit.Key{
					ProjectID:  projectID,
					Repository: repository,
					PRNumber:   pullRequest,
					BaseSHA:    baseSHA,
					HeadSHA:    headSHA,
				})
			} else {
				run, err = backend.LatestSecurityAuditRunForPullRequest(cmd.Context(), projectID, repository, pullRequest)
			}
			if errors.Is(err, store.ErrNotFound) {
				return WrapValidation(errors.New("no trusted security audit evidence found"))
			}
			if err != nil {
				return err
			}
			dispositions, err := backend.ListSecurityAuditDispositions(cmd.Context(), run.ID)
			if err != nil {
				return err
			}
			result := auditEvidenceResultFromRun(run, dispositions)
			return out.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintf(writer, "Audit run %d (%s)\nPR: %s#%d\nBase: %s\nHead: %s\nService: %s\nReviewer: %s (%s)\nAuthentication: %s\nExit: %s\nOutput: %s (%d bytes)\nVerdict: %s\nSummary: %s\n", result.RunID, result.InvocationID, result.Repository, result.PullRequest, result.BaseSHA, result.HeadSHA, result.ServiceIdentity, result.ReviewerVersion, result.ReviewerDigest, result.AuthenticationMode, result.ExitStatus, result.OutputDigest, result.OutputBytes, result.Verdict, result.Summary)
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
	return auditEvidenceResult{
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
}
