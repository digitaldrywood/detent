package artifact

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"
)

func TestTransfersUseOperationDeadline(t *testing.T) {
	for _, side := range []string{"client", "server"} {
		t.Run(side, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				transfer := func(ctx context.Context) error {
					timer := time.NewTimer(31 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if side == "client" {
					client := &Client{Origin: "http://localhost", Token: func(context.Context) (string, error) { return "worker", nil }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if err := transfer(r.Context()); err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
					})}
					if _, err := client.Reserve(ctx, Reservation{}); err != nil {
						t.Fatal(err)
					}
				} else {
					server := &HTTPServer{}
					c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), httptest.NewRecorder())
					if err := server.boundary(func(c echo.Context) error { return transfer(c.Request().Context()) })(c); err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}

func TestListenerReleasesStalledAndRejectedBodies(t *testing.T) {
	for _, scenario := range []string{"invalid bearer stalled reservation", "missing bearer drain", "denied origin drain", "invalid bearer complete reservation", "authorized progressing reservation"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := testService(t)
			h, err := NewHTTPServer(s, &testAuthorizer{})
			if err != nil {
				t.Fatal(err)
			}
			reads := make(chan struct{}, 8)
			h.now = func() time.Time {
				reads <- struct{}{}
				if scenario == "invalid bearer stalled reservation" {
					return time.Now().Add(-time.Minute)
				}
				return time.Now()
			}
			closed := make(chan struct{}, 1)
			server := httptest.NewUnstartedServer(h)
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			server.Start()
			defer server.Close()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			guard, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			wait := func(signal <-chan struct{}) {
				t.Helper()
				select {
				case <-signal:
				case <-guard.Done():
					t.Fatal("listener did not make progress")
				}
			}
			deadline, _ := guard.Deadline()
			if err := conn.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(testReservation(s))
			if err != nil {
				t.Fatal(err)
			}
			token := "invalid"
			if scenario == "authorized progressing reservation" {
				token = "worker"
			}
			headers := "Authorization: Bearer " + token + "\r\n"
			if scenario == "authorized progressing reservation" {
				headers += "Connection: close\r\n"
			}
			if scenario == "missing bearer drain" {
				headers = ""
			}
			if scenario == "denied origin drain" {
				headers += "Origin: https://denied.example.com\r\n"
			}
			if _, err := fmt.Fprintf(conn, "POST /v1/uploads HTTP/1.1\r\nHost: example.com\r\n%sContent-Length: %d\r\n\r\n", headers, len(body)); err != nil {
				t.Fatal(err)
			}
			if scenario == "authorized progressing reservation" {
				wait(reads)
				if _, err := conn.Write(body[:1]); err != nil {
					t.Fatal(err)
				}
				wait(reads)
				if _, err := conn.Write(body[1:]); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "invalid bearer complete reservation" {
				if _, err := conn.Write(body); err != nil {
					t.Fatal(err)
				}
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			want := http.StatusForbidden
			if scenario == "invalid bearer stalled reservation" {
				want = http.StatusUnprocessableEntity
			} else if scenario == "authorized progressing reservation" {
				want = http.StatusCreated
			}
			if response.StatusCode != want {
				t.Fatalf("status=%d want=%d", response.StatusCode, want)
			}
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			wait(closed)
		})
	}
}
