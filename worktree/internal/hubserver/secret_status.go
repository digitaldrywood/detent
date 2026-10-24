package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

type SecretVersionCount struct {
	Table            string `json:"table"`
	Kind             string `json:"kind"`
	MasterKeyVersion int    `json:"master_key_version"`
	Count            int    `json:"count"`
}

type SecretRowProblem struct {
	Database         string `json:"database"`
	OrganizationID   string `json:"organization_id"`
	ProjectID        string `json:"project_id"`
	Kind             string `json:"kind"`
	MasterKeyVersion int    `json:"master_key_version"`
	UpdatedAt        string `json:"updated_at"`
	Problem          string `json:"problem"`
}

type SecretDatabaseStatus struct {
	Database string               `json:"database"`
	Counts   []SecretVersionCount `json:"counts"`
	Problems []SecretRowProblem   `json:"problems"`
	Problem  string               `json:"problem,omitempty"`
}

func SecretStatus(ctx context.Context, path string, keys *hubsecrets.Keyring) (status SecretDatabaseStatus) {
	status = SecretDatabaseStatus{Database: path, Counts: []SecretVersionCount{}, Problems: []SecretRowProblem{}}
	absolute, err := filepath.Abs(path)
	if err != nil {
		status.Problem = "unreadable_database"
		return status
	}
	u := sqliteFileURL(absolute)
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(2500)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		status.Problem = "unreadable_database"
		return status
	}
	defer func() {
		if err := db.Close(); err != nil {
			status.Problem = "unreadable_database"
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		status.Problem = "unreadable_database"
		return status
	}
	rows, err := readSecretRows(ctx, db)
	if err != nil {
		status.Problem = "unreadable_secret_tables"
		return status
	}
	counts := make(map[SecretVersionCount]int)
	for _, row := range rows {
		group := SecretVersionCount{Table: row.table, Kind: row.kind, MasterKeyVersion: row.envelope.Version}
		index, exists := counts[group]
		if !exists {
			index = len(status.Counts)
			counts[group] = index
			status.Counts = append(status.Counts, group)
		}
		status.Counts[index].Count++
		problem := ""
		switch err := keys.Check(row.envelope, secretAAD(row.organization, row.project, row.kind)); {
		case errors.Is(err, hubsecrets.ErrUnavailable):
			problem = "missing_key"
		case err != nil:
			problem = "unwrap_failed"
		case row.envelope.Version != keys.Version():
			problem = "not_active_version"
		}
		if problem != "" {
			status.Problems = append(status.Problems, SecretRowProblem{
				Database: path, OrganizationID: row.organization, ProjectID: row.project,
				Kind: row.kind, MasterKeyVersion: row.envelope.Version,
				UpdatedAt: row.updatedAt, Problem: problem,
			})
		}
	}
	return status
}
