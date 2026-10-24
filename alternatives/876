package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

type AuthSecretCount struct {
	Kind    string `json:"kind"`
	Format  string `json:"format"`
	Version int    `json:"master_key_version"`
	Count   int    `json:"count"`
}

type AuthSecretProblem struct {
	Kind    string `json:"kind"`
	Version int    `json:"master_key_version"`
	Problem string `json:"problem"`
}

type AuthSecretDatabaseStatus struct {
	Database                   string              `json:"database"`
	Counts                     []AuthSecretCount   `json:"counts"`
	Problems                   []AuthSecretProblem `json:"problems"`
	ClientsWithRedirectURIs    int                 `json:"oauth_clients_with_redirect_uris"`
	ClientsWithoutRedirectURIs int                 `json:"oauth_clients_without_redirect_uris"`
	Problem                    string              `json:"problem,omitempty"`
}

func AuthSecretStatus(ctx context.Context, path string, keys *hubsecrets.Keyring) (status AuthSecretDatabaseStatus) {
	status = AuthSecretDatabaseStatus{Database: path, Counts: []AuthSecretCount{}, Problems: []AuthSecretProblem{}}
	absolute, err := filepath.Abs(path)
	if err != nil {
		status.Problem = "unreadable_database"
		return status
	}
	databasePath := filepath.ToSlash(absolute)
	if len(databasePath) >= 3 && databasePath[1] == ':' && databasePath[2] == '/' {
		databasePath = "/" + databasePath
	}
	u := &url.URL{Scheme: "file", Path: databasePath}
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
	if err := readAuthSecretStatus(ctx, db, keys, &status); err != nil {
		status.Problem = "unreadable_auth_tables"
	}
	return status
}

func readAuthSecretStatus(ctx context.Context, db *sql.DB, keys *hubsecrets.Keyring, status *AuthSecretDatabaseStatus) error {
	var hasGitHubConnections bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='github_user_connections')`).Scan(&hasGitHubConnections); err != nil {
		return err
	}
	query := `SELECT binding,'provider-access',access_token FROM authorizations UNION ALL SELECT binding,'provider-refresh',refresh_token FROM authorizations`
	if hasGitHubConnections {
		query += ` UNION ALL SELECT owner_subject,'github-user-access',access_token FROM github_user_connections`
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY 1,2`)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := make(map[AuthSecretCount]int)
	for rows.Next() {
		var binding, kind, value string
		if err := rows.Scan(&binding, &kind, &value); err != nil {
			return err
		}
		group := AuthSecretCount{Kind: kind, Format: "legacy"}
		problem := "legacy"
		switch {
		case value == "":
			group.Format, problem = "empty", ""
		case strings.HasPrefix(value, "hk1."):
			group.Format = "hk1"
			envelope, err := decodeEntrySecret(value)
			group.Version = envelope.Version
			if err == nil {
				err = keys.Check(envelope, entrySecretAAD(kind, binding))
			}
			switch {
			case errors.Is(err, hubsecrets.ErrUnavailable):
				problem = "missing_key"
			case err != nil:
				problem = "unwrap_failed"
			case envelope.Version != keys.Version():
				problem = "not_active_version"
			default:
				problem = ""
			}
		}
		index, exists := counts[group]
		if !exists {
			index = len(status.Counts)
			counts[group] = index
			status.Counts = append(status.Counts, group)
		}
		status.Counts[index].Count++
		if problem != "" {
			status.Problems = append(status.Problems, AuthSecretProblem{Kind: kind, Version: group.Version, Problem: problem})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return db.QueryRowContext(ctx, `SELECT count(CASE WHEN EXISTS(SELECT 1 FROM oauth_client_redirect_uris u WHERE u.client_id=c.id) THEN 1 END),count(CASE WHEN NOT EXISTS(SELECT 1 FROM oauth_client_redirect_uris u WHERE u.client_id=c.id) THEN 1 END) FROM oauth_clients c`).Scan(&status.ClientsWithRedirectURIs, &status.ClientsWithoutRedirectURIs)
}
