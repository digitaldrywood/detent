package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http/httptest"
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
		name, subject, body, html string
		images                    []InlineImage
		magicLink                 bool
		implicitTLS               bool
		untrusted                 bool
	}{
		{name: "finding", subject: "[Detent] runner_heartbeat_gap: runner example", body: "Finding opened\nSummary: Heartbeat gap.\n"},
		{name: "implicit TLS", subject: "New sign-up: user@example.test", body: "New sign-up", implicitTLS: true},
		{name: "untrusted implicit TLS", implicitTLS: true, untrusted: true},
		{name: "unicode", subject: "[Detent] signal: café", body: "Summary: café.\n"},
		{name: "html", subject: "New sign-up: café@example.test", body: "Email: café@example.test\nhttps://example.test/user\n", html: "<h1>New sign-up</h1>\n<p>café &amp; tea</p>"},
		{name: "html implicit TLS with image", subject: "New organization: Example", body: "Organization: Example\n", html: "<h1>Example</h1><img src=\"cid:mark\">", implicitTLS: true, images: []InlineImage{{ContentID: "mark", ContentType: "image/png", Data: []byte(strings.Repeat("fixture image bytes", 10))}}},
		{name: "sign in", subject: "Your Detent sign-in link", body: "https://detent.example.test/auth/link?token=fixture", magicLink: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var roots *x509.CertPool
			if test.implicitTLS {
				server := httptest.NewTLSServer(nil)
				t.Cleanup(server.Close)
				listener = tls.NewListener(listener, server.TLS.Clone())
				roots = x509.NewCertPool()
				if !test.untrusted {
					roots.AddCert(server.Certificate())
				}
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
			if test.implicitTLS {
				tlsSender, err := NewSMTPSender(SMTPConfig{Host: host, Port: 465, From: "detent@example.test"})
				if err != nil {
					t.Fatal(err)
				}
				transport := tlsSender.(*smtpSender)
				transport.port = port
				transport.tlsConfig.RootCAs = roots
				sender = transport
			}
			if test.magicLink {
				err = sender.SendMagicLink(t.Context(), Message{To: "owner@example.test", URL: test.body, ExpiresAt: time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)})
			} else {
				err = sender.SendEmail(t.Context(), EmailMessage{To: "owner@example.test", Subject: test.subject, Body: test.body, HTMLBody: test.html, InlineImages: test.images})
			}
			if test.untrusted {
				if err == nil || !strings.Contains(err.Error(), "certificate") {
					t.Fatalf("untrusted SMTP certificate error=%v", err)
				}
				if result := <-completed; result.err == nil {
					t.Fatal("untrusted TLS connection delivered a message")
				}
				return
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
			if test.html != "" {
				kind, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
				if err != nil || kind != "multipart/alternative" || params["boundary"] == "" {
					t.Fatalf("alternative header=%q error=%v", message.Header.Get("Content-Type"), err)
				}
				second, err := emailMessage("detent@example.test", "owner@example.test", EmailMessage{HTMLBody: test.html})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(second), params["boundary"]) {
					t.Fatal("multipart boundary reused")
				}
				parts := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
				textPart, err := parts.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				assertEmailPart(t, textPart, "text/plain", test.body)
				htmlPart, err := parts.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				if len(test.images) > 0 {
					kind, relatedParams, err := mime.ParseMediaType(htmlPart.Header.Get("Content-Type"))
					if err != nil || kind != "multipart/related" || relatedParams["boundary"] == params["boundary"] {
						t.Fatalf("related header=%q error=%v", htmlPart.Header.Get("Content-Type"), err)
					}
					related := multipart.NewReader(htmlPart, relatedParams["boundary"])
					htmlPart, err = related.NextPart()
					if err != nil {
						t.Fatal(err)
					}
					assertEmailPart(t, htmlPart, "text/html", test.html)
					for _, image := range test.images {
						part, err := related.NextPart()
						if err != nil {
							t.Fatal(err)
						}
						data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
						if err != nil || string(data) != string(image.Data) || part.Header.Get("Content-ID") != "<"+image.ContentID+">" || part.Header.Get("Content-Type") != image.ContentType || part.Header.Get("Content-Disposition") != "inline" {
							t.Fatalf("inline image=%+v data=%q error=%v", part.Header, data, err)
						}
					}
					if _, err := related.NextPart(); err != io.EOF {
						t.Fatalf("related ending=%v", err)
					}
				} else {
					assertEmailPart(t, htmlPart, "text/html", test.html)
				}
				if _, err := parts.NextPart(); err != io.EOF {
					t.Fatalf("alternative ending=%v", err)
				}
				body = []byte(test.body)
			}
			if subject != test.subject || message.Header.Get("To") != "owner@example.test" || message.Header.Get("From") != "detent@example.test" || !strings.Contains(strings.ReplaceAll(string(body), "\r\n", "\n"), test.body) {
				t.Fatalf("unexpected email: %s", result.raw)
			}
		})
	}
}

func assertEmailPart(t *testing.T, part *multipart.Part, wantType, wantBody string) {
	t.Helper()
	kind, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
	if err != nil || kind != wantType || params["charset"] != "UTF-8" {
		t.Fatalf("part header=%q error=%v", part.Header.Get("Content-Type"), err)
	}
	body, err := io.ReadAll(part)
	if err != nil || strings.ReplaceAll(string(body), "\r\n", "\n") != wantBody {
		t.Fatalf("part body=%q want %q error=%v", body, wantBody, err)
	}
}

func TestEmailMessagePlainText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ body, wireBody string }{
		{body: "hello\nworld\r\n", wireBody: "hello\r\nworld\r\n"},
		{body: "café & tea\r\n", wireBody: "café & tea\r\n"},
	} {
		t.Run(test.body, func(t *testing.T) {
			got, err := emailMessage("sender@example.test", "user@example.test", EmailMessage{Subject: "Notice", Body: test.body})
			want := "From: sender@example.test\r\nTo: user@example.test\r\nSubject: Notice\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + test.wireBody
			if err != nil || string(got) != want {
				t.Fatalf("message=%q want %q error=%v", got, want, err)
			}
		})
	}
}

func TestEmailMessageInlineHeaders(t *testing.T) {
	t.Parallel()
	for _, image := range []InlineImage{
		{ContentID: "mark\r\nBcc: other@example.test", ContentType: "image/png"},
		{ContentID: "mark", ContentType: "image/png\r\nBcc: other@example.test"},
		{ContentType: "image/png"},
	} {
		t.Run(image.ContentID+image.ContentType, func(t *testing.T) {
			if _, err := emailMessage("sender@example.test", "user@example.test", EmailMessage{HTMLBody: "<p>Example</p>", InlineImages: []InlineImage{image}}); err == nil {
				t.Fatal("invalid inline image headers accepted")
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
