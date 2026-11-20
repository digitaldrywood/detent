package sessiontimeout

import (
	"context"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestActivityContinuesAfterReplayCacheFills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session := Start(t.Context(), 5*time.Minute, time.Hour, nil)
		defer session.Close()
		active := session.StartCommand("already-running")
		for i := range 2 * maxEvents {
			session.Event(strconv.Itoa(i))
		}
		if len(session.seen) > maxEvents {
			t.Fatalf("replay cache retained %d events, want at most %d", len(session.seen), maxEvents)
		}
		if !active || session.StartCommand("already-running") || !session.EndCommand("already-running") {
			t.Fatal("replay turnover lost the active command lifecycle")
		}
		<-time.After(4 * time.Minute)
		fresh := session.Event("new-current-event")
		replay := session.Event("new-current-event")
		<-time.After(2 * time.Minute)
		synctest.Wait()
		if !fresh || replay || session.Context().Err() != nil {
			t.Fatalf("fresh=%v replay=%v cause=%v", fresh, replay, context.Cause(session.Context()))
		}
		if !session.StartCommand("new-bounded-command") {
			t.Fatal("a full replay cache rejected a new command")
		}
		<-time.After(6 * time.Minute)
		synctest.Wait()
		if session.Context().Err() != nil || !session.EndCommand("new-bounded-command") {
			t.Fatalf("new bounded command did not survive idle deadline: %v", context.Cause(session.Context()))
		}
		if session.StartCommand("new-bounded-command") || session.EndCommand("new-bounded-command") {
			t.Fatal("recent completed command replay reopened its lifecycle")
		}
	})
}
