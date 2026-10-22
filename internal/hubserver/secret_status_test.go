package hubserver

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecretStatusLiveDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	seedHubDatabaseImage(t, path, true)
	db, err := openDatabase(t.Context(), (Config{DatabasePath: path}).normalized())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	var organization string
	if err := db.reader.QueryRowContext(t.Context(), "SELECT id FROM organizations WHERE local=1").Scan(&organization); err != nil {
		t.Fatal(err)
	}
	keys := secretTestKeys(t, "2", "1", "2")
	envelope, err := keys.Seal([]byte("known-live-token"), secretAAD(organization, "", slackWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(t.Context(), `INSERT INTO organization_secrets (organization_id, kind, ciphertext, nonce, wrapped_data_key, master_key_version, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, organization, slackWebhookSecret, envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, testTimestamp); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.reader.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	if _, err := tx.ExecContext(t.Context(), "UPDATE organization_secrets SET master_key_version=1 WHERE organization_id=?", organization); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string][]byte {
		t.Helper()
		files := map[string][]byte{}
		for _, suffix := range []string{"", "-wal", ".lock"} {
			contents, err := os.ReadFile(path + suffix)
			if err != nil {
				t.Fatal(err)
			}
			files[suffix] = contents
		}
		return files
	}
	before := snapshot()
	status := SecretStatus(t.Context(), path, keys)
	if status.Problem != "" || len(status.Problems) != 0 || len(status.Counts) != 1 || status.Counts[0].MasterKeyVersion != 2 || status.Counts[0].Count != 1 {
		t.Fatalf("live status=%+v", status)
	}
	for suffix, contents := range snapshot() {
		if !bytes.Equal(contents, before[suffix]) {
			t.Fatalf("status modified database file %q", suffix)
		}
	}
	var afterVersion int
	if err := db.reader.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&afterVersion); err != nil || afterVersion != version {
		t.Fatalf("user_version changed from %d to %d: %v", version, afterVersion, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}
