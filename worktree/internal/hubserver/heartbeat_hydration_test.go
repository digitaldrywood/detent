package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestHeartbeatHydratesArchivedPayloadBeforeWriterRetry(t *testing.T) {
	store := &memoryRunDetails{}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{RunDetails: store, now: func() time.Time { return now }})
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	runner.enroll(t)
	f.service.tokenUseWork.Wait()

	archiveContext := f.service.contentContext(t.Context())
	ref, err := prepareMeteredContent(archiveContext, string(f.project.OrganizationID), []byte(`{"value":"archived"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "CREATE TABLE heartbeat_hydration_writes (writes INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO heartbeat_hydration_writes (writes) VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	store.beforeRead = func(ctx context.Context) error {
		content, ok := ctx.Value(changeContentKey{}).(*changeContent)
		if !ok {
			return errors.New("missing content context")
		}
		content.mu.Lock()
		writing := content.writing
		content.mu.Unlock()
		if writing {
			return errors.New("payload read inside writer")
		}
		return nil
	}

	request := httptest.NewRequest(http.MethodPost, "/test/heartbeat", nil)
	request = request.WithContext(f.service.runDetailContext(request.Context()))
	recorder := httptest.NewRecorder()
	c := f.service.echo.NewContext(request, recorder)
	c.SetPath("/test/heartbeat")
	credential := apiCredential{ID: runner.binding.RunnerID, Hash: apikey.HashToken(runner.redemption.Credential), Scope: apiScopeWorker, NativeOnly: true, Runner: runner.identity}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	c.Set("hub_api_credential", credential)
	c.Set("native_scope", scope)
	responseReads := 0
	err = f.service.runnerTransaction(c, http.StatusOK, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		var archived map[string]string
		raw, err := readStoredPayload(ctx, ref.Key)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &archived); err != nil {
			return nil, err
		}
		if archived["value"] != "archived" {
			return nil, errors.New("hydrated payload differs")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE heartbeat_hydration_writes SET writes = writes + 1"); err != nil {
			return nil, err
		}
		if err := updateRunnerHeartbeat(ctx, tx, scope, 2, "test", "", "", now); err != nil {
			return nil, err
		}
		return runnerCommittedResponse(func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
			responseReads++
			if responseReads == 1 {
				return nil, &claimHydration{run: func(context.Context, nativeQueryer) error { return nil }}
			}
			var writes int
			if err := tx.QueryRowContext(ctx, "SELECT writes FROM heartbeat_hydration_writes").Scan(&writes); err != nil {
				return nil, err
			}
			if writes != 1 {
				return nil, errors.New("heartbeat transaction was replayed after commit")
			}
			return archived, nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("heartbeat status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if responseReads != 2 {
		t.Fatalf("committed response reads=%d, want 2", responseReads)
	}
	var rawHeartbeat string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT last_heartbeat_at FROM runner_identities WHERE id=?", runner.binding.RunnerID).Scan(&rawHeartbeat); err != nil {
		t.Fatal(err)
	}
	heartbeat, err := parseTimeValue(rawHeartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if !heartbeat.Equal(now) {
		t.Fatalf("last heartbeat=%s, want %s", heartbeat, now)
	}
	store.mu.Lock()
	writerReads := store.writerReads
	store.mu.Unlock()
	if writerReads != 0 {
		t.Fatalf("read archived payload %d times while holding the writer", writerReads)
	}
}
