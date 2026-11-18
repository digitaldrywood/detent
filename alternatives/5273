package workspace

import (
	"context"

	"github.com/digitaldrywood/detent/internal/workspace/signing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func (l *LocalGit) CommitReport() CommitReport {
	report := l.commits.Report
	if l.commits.Unavailable != nil && l.commits.Unavailable.Load() {
		report.Signing = "unsigned"
	}
	return report
}

func (l *LocalGit) Problems() []runnerauth.Problem {
	return l.CommitReport().Problems()
}

func (l *LocalGit) runCommit(ctx context.Context, path string, env []string, args ...string) (string, error) {
	policy := l.commits
	if policy.Report.Signing != "unsigned" {
		if attempted, err := policy.VerifySigning(ctx); err != nil {
			if !attempted || ctx.Err() != nil || !signing.SignerUnavailable(err) {
				return "", err
			}
			policy.Report.Signing = "unsigned"
			if policy.Unavailable != nil {
				policy.Unavailable.Store(true)
			}
			l.logger.Warn("commit signing unavailable", "signing", "unsigned", "error", commandErrorOutput(err))
		} else if policy.Unavailable != nil {
			policy.Unavailable.Store(false)
		}
	}
	return policy.Run(ctx, path, env, args...)
}

type CommitIdentity = signing.Identity
type CommitReport = signing.Report
type commitPolicy = signing.Policy

const commitSigningFailureMarker = signing.SigningFailureMarker

func probeCommitPolicy(ctx context.Context, source string, fallback CommitIdentity) (commitPolicy, error) {
	return signing.Probe(ctx, source, fallback)
}
func ProbeCommitReport(ctx context.Context, source string, fallback CommitIdentity) (CommitReport, error) {
	return signing.ProbeReport(ctx, source, fallback)
}
