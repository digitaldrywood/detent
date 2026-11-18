package cloudentry

import (
	"context"
	"errors"
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

func readAuthSecretStatus(ctx context.Context, db *entryDatabase, keys *hubsecrets.Keyring, status *AuthSecretDatabaseStatus) (resultErr error) {
	var hasGitHubConnections bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('platform.github_user_connections') IS NOT NULL`).Scan(&hasGitHubConnections); err != nil {
		return err
	}
	query := `SELECT binding,'provider-access',access_token FROM platform.authorizations UNION ALL SELECT binding,'provider-refresh',refresh_token FROM platform.authorizations`
	if hasGitHubConnections {
		query += ` UNION ALL SELECT owner_subject,'github-user-access',access_token FROM platform.github_user_connections`
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY 1,2`)
	if err != nil {
		return err
	}
	defer closeEntryRows(rows, &resultErr)
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
	return db.QueryRowContext(ctx, `SELECT count(CASE WHEN EXISTS(SELECT 1 FROM platform.oauth_client_redirect_uris u WHERE u.client_id=c.id) THEN 1 END),count(CASE WHEN NOT EXISTS(SELECT 1 FROM platform.oauth_client_redirect_uris u WHERE u.client_id=c.id) THEN 1 END) FROM platform.oauth_clients c`).Scan(&status.ClientsWithRedirectURIs, &status.ClientsWithoutRedirectURIs)
}

func (s *Service) AuthSecretStatus(ctx context.Context) AuthSecretDatabaseStatus {
	status := AuthSecretDatabaseStatus{Database: s.config.Storage.Backend, Counts: []AuthSecretCount{}, Problems: []AuthSecretProblem{}}
	if err := readAuthSecretStatus(ctx, s.auth.store.reader, s.auth.keys, &status); err != nil {
		status.Problem = "unreadable_auth_tables"
	}
	return status
}
