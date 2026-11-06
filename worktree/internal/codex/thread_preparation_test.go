package codex

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAppServerThreadPreparation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"verify", "resume", "fresh"} {
		for _, fail := range []bool{false, true} {
			name := mode + "/success"
			if fail {
				name = mode + "/prepare error"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				requestID, method := threadResumeRequestID, "thread/resume"
				switch mode {
				case "verify":
					requestID, method = threadReadRequestID, "thread/read"
				case "fresh":
					requestID, method = threadStartRequestID, "thread/start"
				}
				messages := []Message{
					responseMessage(t, initializeRequestID, `{"userAgent":"codex-test"}`),
					rolloutResponseMessage(t, requestID, `{"thread":{"id":"thread-existing"}}`, nil),
				}
				if mode != "verify" {
					messages = append(messages,
						responseMessage(t, turnStartRequestID, `{"turn":{"id":"turn-1"}}`),
						notificationMessage(t, "turn/completed", `{"threadId":"thread-existing","turn":{"id":"turn-1","status":"completed"}}`))
				}
				transport := newFakeAppServerTransport(messages)
				prepared := false
				started := false
				prepareErr := errors.New("cannot copy legacy rollout")
				server, err := NewAppServer(threadPreparationFactory{start: func() Transport {
					started = true
					if mode != "fresh" && !prepared {
						t.Fatal("app-server started before legacy rollout preparation")
					}
					return transport
				}}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second),
					WithThreadPreparation(func(ctx context.Context, id string) error {
						if id != "thread-existing" || ctx == nil {
							t.Fatalf("preparation arguments = %v, %q", ctx, id)
						}
						prepared = true
						if fail {
							return prepareErr
						}
						return nil
					}))
				if err != nil {
					t.Fatal(err)
				}
				if mode == "verify" {
					err = server.VerifyThread(t.Context(), " thread-existing ")
				} else {
					threadID := " thread-existing "
					if mode == "fresh" {
						threadID = ""
					}
					_, err = server.RunTurn(t.Context(), RunTurnRequest{ResumeThreadID: threadID, Model: "gpt-test", Prompt: "Continue"}, nil)
				}
				if fail && mode != "fresh" {
					if !errors.Is(err, prepareErr) || started {
						t.Fatalf("preparation failure = %v, started = %v", err, started)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if mode == "fresh" && prepared {
					t.Fatal("fresh thread copied legacy history")
				}
				assertRequest(t, transport.sentMessages()[2], requestID, method)
			})
		}
	}
}

type threadPreparationFactory struct {
	start func() Transport
}

func (f threadPreparationFactory) NewTransport(context.Context) (Transport, error) {
	return f.start(), nil
}
