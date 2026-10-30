package hubserver

import (
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestConversationPublicationRejectsChangedPreparation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		update string
	}{
		{name: "unchanged"},
		{name: "revision changed", update: "UPDATE conversations SET revision=revision+1 WHERE id=?"},
		{name: "events changed", update: "UPDATE conversations SET event_seq=event_seq+1 WHERE id=?"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConversationAPIFixture(t, nil)
			record := f.create(t, f.token, map[string]any{"title": "Prepared conversation"}).Conversation
			chat := f.service.conversations
			plan, err := chat.newPublicationPlan(t.Context(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.update != "" {
				if _, err := f.service.database.db.ExecContext(t.Context(), test.update, record.ID); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err = chat.transact(plan.context(t.Context()), func(*sql.Tx, time.Time) error {
				called = true
				return nil
			})
			if test.update == "" {
				if err != nil || !called {
					t.Fatalf("unchanged preparation: called=%t error=%v", called, err)
				}
				return
			}
			var conflict *nativeError
			if called || !errors.As(err, &conflict) || conflict.Code != "revision_conflict" || conflict.status != http.StatusConflict {
				t.Fatalf("changed preparation: called=%t error=%v, want revision_conflict", called, err)
			}
		})
	}
}
