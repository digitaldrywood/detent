package auth

import (
	"context"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSMTPMessages(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, subject, body string
		magicLink           bool
	}{
		{name: "finding", subject: "[Detent] runner_heartbeat_gap: runner example", body: "Finding opened\nSummary: Heartbeat gap.\n"},
		{name: "unicode", subject: "[Detent] signal: café", body: "Summary: café.\n"},
		{name: "sign in", subject: "Your Detent sign-in link", body: "https://detent.example.test/auth/link?token=fixture", magicLink: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := listener.Close(); err != nil {
					t.Error(err)
				}
			})
			type receipt struct {
				raw string
				err error
			}
			completed := make(chan receipt, 1)
			go func() {
				raw, err := receiveSMTPMessage(listener)
				completed <- receipt{raw, err}
			}()
			host, portText, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			sender, err := NewSMTPSender(SMTPConfig{Host: host, Port: port, From: "detent@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			if test.magicLink {
				err = sender.SendMagicLink(t.Context(), Message{To: "owner@example.test", URL: test.body, ExpiresAt: time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)})
			} else {
				err = sender.SendEmail(t.Context(), EmailMessage{To: "owner@example.test", Subject: test.subject, Body: test.body})
			}
			if err != nil {
				t.Fatal(err)
			}
			result := <-completed
			if result.err != nil {
				t.Fatal(result.err)
			}
			message, err := mail.ReadMessage(strings.NewReader(result.raw))
			if err != nil {
				t.Fatal(err)
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(message.Body)
			if err != nil {
				t.Fatal(err)
			}
			if subject != test.subject || message.Header.Get("To") != "owner@example.test" || message.Header.Get("From") != "detent@example.test" || !strings.Contains(strings.ReplaceAll(string(body), "\r\n", "\n"), test.body) {
				t.Fatalf("unexpected email: %s", result.raw)
			}
		})
	}
}

func receiveSMTPMessage(listener net.Listener) (string, error) {
	connection, err := listener.Accept()
	if err != nil {
		return "", err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	protocol := textproto.NewConn(connection)
	if err := protocol.PrintfLine("220 localhost ESMTP"); err != nil {
		return "", err
	}
	var message string
	for {
		command, err := protocol.ReadLine()
		if err != nil {
			return "", err
		}
		switch {
		case strings.HasPrefix(command, "EHLO "), strings.HasPrefix(command, "HELO "), strings.HasPrefix(command, "MAIL FROM:"), strings.HasPrefix(command, "RCPT TO:"):
			err = protocol.PrintfLine("250 OK")
		case command == "DATA":
			if err := protocol.PrintfLine("354 Send message"); err != nil {
				return "", err
			}
			raw, readErr := protocol.ReadDotBytes()
			if readErr != nil {
				return "", readErr
			}
			message = string(raw)
			err = protocol.PrintfLine("250 OK")
		case command == "QUIT":
			return message, protocol.PrintfLine("221 Bye")
		default:
			err = protocol.PrintfLine("500 Unexpected command")
		}
		if err != nil {
			return "", err
		}
	}
}

func TestSMTPRejectsInvalidHeaders(t *testing.T) {
	t.Parallel()
	sender, err := NewSMTPSender(SMTPConfig{Host: "127.0.0.1", Port: 1, From: "detent@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, to, subject, want string }{
		{"subject injection", "owner@example.test", "hello\r\nBcc: other@example.test", "email headers are invalid"},
		{"recipient injection", "owner@example.test\r\nBcc: other@example.test", "hello", "email headers are invalid"},
		{"invalid recipient", "invalid", "hello", "email recipient is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := sender.SendEmail(ctx, EmailMessage{To: test.to, Subject: test.subject, Body: "body"})
			if err == nil || err.Error() != test.want {
				t.Fatalf("error=%v want %s", err, test.want)
			}
		})
	}
}
