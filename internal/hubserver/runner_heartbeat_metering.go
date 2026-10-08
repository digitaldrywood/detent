package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// heartbeatMetering meters a runner heartbeat against the hosted allowances
// without scanning tenant history inside the writer transaction. The
// heartbeat only ever appends a few rows, so ingested events are measured by
// the collaboration event rowid high-water mark, and collaboration bytes are
// the reader-side census plus the bytes the heartbeat's configuration receipt
// changes.
type heartbeatMetering struct {
	metrics      []string
	census       *int64
	command      *heartbeatCommandKey
	commandBytes *int64
}

type heartbeatCommandKey struct {
	actor, operation, key string
}

// prepareHeartbeatMetering runs before the writer transaction. The byte
// census reads the whole tenant, so it runs on the reader pool and only when
// a configuration receipt is pending for this runner and project.
func (s *Service) prepareHeartbeatMetering(ctx context.Context, scope nativeScope, metrics []string) (heartbeatMetering, error) {
	metering := heartbeatMetering{metrics: slices.DeleteFunc(slices.Clone(metrics), func(name string) bool {
		return name == "events_total" || name == "collaboration_bytes"
	})}
	if s.database.hostedPlans == nil || scope.credential.Runner.RunnerID == "" {
		return metering, nil
	}
	var raw string
	err := s.database.reader.QueryRowContext(ctx, "SELECT routing_settings_json FROM runner_identities WHERE organization_id = ? AND id = ?", scope.organization, scope.credential.Runner.RunnerID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return metering, nil
	}
	if err != nil {
		return metering, err
	}
	var settings runnerSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return metering, err
	}
	command := settings.ProjectConfigurationCommand
	if command == nil || command.Request.ProjectID != string(scope.project) {
		return metering, nil
	}
	var issuer apiCredential
	if json.Unmarshal(command.Issuer, &issuer) == nil {
		metering.command = &heartbeatCommandKey{actor: issuer.ID, operation: command.Request.Operation + " " + scope.credential.Runner.RunnerID + " " + string(scope.project), key: command.Request.RequestID}
	}
	now, err := s.database.currentTime()
	if err != nil {
		return metering, err
	}
	census, err := s.database.hostedConsumption(ctx, s.database.reader, now, "collaboration_bytes")
	if err != nil {
		return metering, err
	}
	value := census["collaboration_bytes"]
	metering.census = &value
	return metering, nil
}

// consumption reads the bounded heartbeat metrics inside the writer
// transaction. The first call records the configuration receipt's bytes so
// the second reports the census plus only what the heartbeat changed.
func (m *heartbeatMetering) consumption(ctx context.Context, d *database, tx *sql.Tx, organization tracker.OrganizationID, now time.Time) (map[string]int64, error) {
	result, err := d.hostedConsumption(ctx, tx, now, m.metrics...)
	if err != nil {
		return nil, err
	}
	if d.hostedPlans == nil {
		return result, nil
	}
	var mark int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(rowid), 0) FROM collaboration_events").Scan(&mark); err != nil {
		return nil, err
	}
	result["events_total"] = mark
	if m.census != nil {
		bytes, err := m.readCommandBytes(ctx, tx, organization)
		if err != nil {
			return nil, err
		}
		if m.commandBytes == nil {
			m.commandBytes = &bytes
		}
		result["collaboration_bytes"] = *m.census + bytes - *m.commandBytes
	}
	return result, nil
}

func (m *heartbeatMetering) readCommandBytes(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID) (int64, error) {
	if m.command == nil {
		return 0, nil
	}
	var bytes int64
	err := tx.QueryRowContext(ctx, "SELECT coalesce(sum(length(CAST(response_json AS BLOB))), 0) FROM native_commands WHERE organization_id = ? AND actor_id = ? AND operation = ? AND command_key = ?",
		organization, m.command.actor, m.command.operation, m.command.key).Scan(&bytes)
	return bytes, err
}
