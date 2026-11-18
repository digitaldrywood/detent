package publication_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMain(m, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return errors.Join(scenariotest.ClosePostgres(), testsupport.CloseNativeChangePostgres(ctx), testsupport.CloseDispatchPostgres(ctx))
	}))
}
func TestRunnerPublication(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "held publication reconciles lost create through new Runner lease", Publication: "lost_create", Rework: true, Formal: true},
		{Name: "ambiguous retained PR push followed by third party advancement executes Rework", Publication: "lost_push", PublicationFault: "advanced_pr", Rework: true, Formal: true},
		{Name: "first publication HEAD discovery failure retries infrastructure", Publication: "head_read", Rework: true, Formal: true},
		{Name: "uncertain publication initial ref read failure retries infrastructure", Publication: "lost_push", PublicationFault: "initial_read", Rework: true, Formal: true},
		{Name: "uncertain publication post push ref read failure retries infrastructure", Publication: "lost_push", PublicationFault: "post_push_read", Rework: true, Formal: true},
		{Name: "held publication reconciles lost push through new Runner lease", Publication: "lost_push", Rework: true, Formal: true},
		{Name: "held implementation publication reconciles lost push on another Runner", Publication: "lost_push", ForeignRunner: true, Staged: true},
		{Name: "held implementation publication refreshes stale policy on another Runner", Publication: "lost_push", PublicationFault: "policy", ForeignRunner: true, StalePolicy: true, Staged: true},
		{Name: "held publication records a fresh validator decision on new Runner lease", Publication: "lost_create", Rework: true, Formal: true, Validator: "pass"},
		{Name: "confirmed publication retries pending validator on unchanged source", Publication: "confirmed", Rework: true, Validator: "wait"},
		{Name: "confirmed publication retries validator rework after prerequisite repair", Publication: "confirmed", Rework: true, Validator: "rework"},
		{Name: "confirmed publication reuses applicable passing native verdict", Publication: "confirmed", Rework: true, Validator: "pass"},
		{Name: "confirmed publication preserves human hold during validator recovery", Publication: "confirmed", Rework: true, Formal: true, Validator: "rework"},
		{Name: "held publication preserves validator authority across configuration changes", Publication: "lost_create", PublicationFault: "policy", Rework: true, Formal: true, Validator: "pass"},
		{Name: "held publication refuses stale validator head", Publication: "lost_create", PublicationFault: "changed_head", Rework: true, Formal: true, Validator: "pass"},
		{Name: "held publication reconciles staged work through new Runner lease", Publication: "lost_create", Rework: true, Formal: true, Staged: true},
		{Name: "held publication refuses replaced version on new Runner lease", Publication: "lost_create", PublicationFault: "version", Rework: true, Formal: true},
		{Name: "held publication admits a foreign runner to reconcile", Publication: "lost_create", PublicationFault: "foreign_source", Rework: true, Formal: true},
		{Name: "held publication refuses unavailable source on new Runner lease", Publication: "lost_create", PublicationFault: "unavailable_source", Rework: true, Formal: true},
		{Name: "held publication repeats the write for missing_pr on new Runner lease", Publication: "lost_create", PublicationFault: "missing_pr", Rework: true, Formal: true},
		{Name: "held publication refuses wrong_base on new Runner lease", Publication: "lost_create", PublicationFault: "wrong_base", Rework: true, Formal: true},
		{Name: "held publication refuses wrong_repository on new Runner lease", Publication: "lost_create", PublicationFault: "wrong_repository", Rework: true, Formal: true},
		{Name: "held publication refuses wrong_head on new Runner lease", Publication: "lost_create", PublicationFault: "wrong_head", Rework: true, Formal: true},
		{Name: "held publication refuses wrong_branch on new Runner lease", Publication: "lost_create", PublicationFault: "wrong_branch", Rework: true, Formal: true},
		{Name: "held publication refuses multiple_prs on new Runner lease", Publication: "lost_create", PublicationFault: "multiple_prs", Rework: true, Formal: true},
		{Name: "held publication refuses unavailable_forge on new Runner lease", Publication: "lost_create", PublicationFault: "unavailable_forge", Rework: true, Formal: true},
		{Name: "held publication repeats the write for missing_remote on new Runner lease", Publication: "lost_create", PublicationFault: "missing_remote", Rework: true, Formal: true},
		{Name: "held publication refuses dirty_source on new Runner lease", Publication: "lost_create", PublicationFault: "dirty_source", Rework: true, Formal: true},
		{Name: "held publication refuses changed_head on new Runner lease", Publication: "lost_create", PublicationFault: "changed_head", Rework: true, Formal: true},
		{Name: "held publication survives configuration change on new Runner lease", Publication: "lost_create", PublicationFault: "policy", Rework: true, Formal: true},
		{Name: "held publication refuses lease on new Runner lease", Publication: "lost_create", PublicationFault: "lease", Rework: true, Formal: true},
		{Name: "held publication refuses branch on new Runner lease", Publication: "lost_create", PublicationFault: "branch", Rework: true, Formal: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}

func TestNativePublicationFixtureKeepsLeaseUntilHandoff(t *testing.T) {
	scenariotest.PublicationFixtureKeepsLeaseUntilHandoff(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
