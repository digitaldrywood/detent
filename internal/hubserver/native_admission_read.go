package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateNativeAdmissionContext(c tracker.NativeAdmissionContext) error {
	if err := c.Validate(); err != nil {
		return nativeInvalid(err.Error())
	}
	return nil
}

func admissionObservationPath(runner string, project tracker.ProjectID) string {
	return "$.native_admission." + runner + "." + string(project)
}

func prepareRunnerAdmissionObservation(observation *tracker.NativeAdmissionObservation, now time.Time) (string, error) {
	if observation == nil {
		return "", nil
	}
	if observation.RunnerRevision < 1 || !observation.ReceivedAt.IsZero() {
		return "", nativePublicInvalid("Admission observation requires the observed runner revision")
	}
	if err := validateNativeAdmissionContext(observation.Context); err != nil {
		return "", err
	}
	current := *observation
	current.ReceivedAt = now
	raw, err := json.Marshal(current)
	if err != nil {
		return "", err
	}
	if len(raw) > 8192 {
		return "", nativePublicInvalid("Admission observation exceeds its bound")
	}
	return string(raw), nil
}

func storeRunnerAdmissionObservation(ctx context.Context, tx *sql.Tx, scope nativeScope, observation *tracker.NativeAdmissionObservation, now time.Time) error {
	raw, err := prepareRunnerAdmissionObservation(observation, now)
	if err != nil {
		return err
	}
	return storeEncodedRunnerAdmission(ctx, tx, scope, raw)
}

func storeEncodedRunnerAdmission(ctx context.Context, tx *sql.Tx, scope nativeScope, raw string) error {
	if raw == "" {
		return nil
	}
	var observed tracker.NativeAdmissionObservation
	if err := json.Unmarshal([]byte(raw), &observed); err != nil {
		return err
	}
	if err := storeAdmissionFacts(ctx, tx, scope.credential.Runner.RunnerID, scope.project, observed); err != nil {
		return err
	}
	return storeLegacyAdmission(ctx, tx, scope, raw)
}

func readRunnerAdmissionObservation(ctx context.Context, q nativeQueryer, scope nativeScope, runner runnerauth.Runner, policyID string, now time.Time) (*tracker.NativeAdmissionContext, error) {
	observed, err := readAdmissionFacts(ctx, q, runner.RunnerID, scope.project, runner.MachineID, scope.organization)
	if err != nil {
		return nil, err
	}

	if validateNativeAdmissionContext(observed.Context) != nil || observed.RunnerRevision != runner.Revision || observed.ReceivedAt.IsZero() || now.Before(observed.ReceivedAt) || !now.Before(observed.ReceivedAt.Add(runnerauth.HeartbeatTimeout)) || now.Before(observed.Context.ObservedAt) || !now.Before(observed.Context.ObservedAt.Add(runnerauth.HeartbeatTimeout)) {
		return nil, nil //nolint:nilnil // Missing or unusable heartbeat evidence is a valid optional result.
	}
	return &observed.Context, nil
}
