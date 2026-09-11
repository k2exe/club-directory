package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeHeaderValue(t *testing.T) {
	for in, want := range map[string]string{
		"Ordinary subject":               "Ordinary subject",
		"hello\r\nX-Injected: yes":       "helloX-Injected: yes",
		"hello\nBcc: attacker@evil.test": "helloBcc: attacker@evil.test",
		"tab\tkept":                      "tab\tkept",
		"":                               "",
	} {
		if got := sanitizeHeaderValue(in); got != want {
			t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// Regression: a ticket subject is member-controlled and lands straight in a
// notification's Subject: header. Without sanitizing CR/LF, a subject like
// "hi\r\nX-Injected: yes" splits into two raw header lines. This goes
// through the real HTTP path (a.post, not a.store.CreateTicket directly) so
// notifyTicketCreated actually fires and an email genuinely exists to
// inspect — the earlier version of this test called CreateTicket alone,
// never sent anything, and passed vacuously with a loop that ran zero
// times. It also checks for a genuine new header LINE rather than the raw
// substring "X-Injected:", which — precisely because sanitization strips
// the CR/LF that would have started a new line — legitimately still
// appears glued onto the end of the Subject value after sanitizing; that's
// the fix working, not a leak.
func TestRegressionTicketSubjectCannotInjectHeaders(t *testing.T) {
	a := testApp(t)
	role, submitter, _, _, _ := setupTicketFixture(t, a)
	evilSubject := "Need help\r\nX-Injected: yes\r\nBcc: attacker@evil.test"

	form := url.Values{"role_id": {role.ID}, "subject": {evilSubject}, "body": {"details"}}
	if code := a.post(t, "/tickets", form, submitter).Code; code != http.StatusSeeOther {
		t.Fatalf("ticket create returned %d", code)
	}

	files := readOutboxFor(t, a, "webadmin_example.com")
	if len(files) == 0 {
		t.Fatal("expected the supporter to have been emailed — the test would pass vacuously otherwise")
	}
	raw, err := os.ReadFile(filepath.Join(a.store.OutboxDir(), files[0]))
	if err != nil {
		t.Fatal(err)
	}
	// Only the header block (before the blank-line separator) matters here:
	// the body legitimately quotes the ticket subject back to the
	// recipient, newlines and all, so "X-Injected:" appearing THERE is
	// just the subject being displayed, not an injected header.
	headers, _, found := strings.Cut(string(raw), "\r\n\r\n")
	if !found {
		t.Fatal("message has no header/body separator — malformed test fixture")
	}
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "X-Injected:") || strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("a new header line was injected: %q\nfull message:\n%s", line, raw)
		}
	}
}

// Regression: spool filenames used to be only millisecond-timestamp plus
// recipient, so concurrent sends to the same address collided and
// os.WriteFile silently dropped all but the last one written per
// millisecond. Every no-SMTP deployment relies on the outbox retaining
// every message.
func TestRegressionConcurrentSpoolDoesNotDropMessages(t *testing.T) {
	dir := t.TempDir()
	m := &Mailer{Outbox: dir}
	const n = 200
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			m.Send("busy@example.com", fmt.Sprintf("Message %d", i), "body")
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Errorf("got %d spooled files, want %d — concurrent sends overwrote each other", len(entries), n)
	}
}

func TestCreateTicketRejectsOversizedInput(t *testing.T) {
	a := testApp(t)
	role, submitter, _, _, _ := setupTicketFixture(t, a)
	huge := make([]byte, maxTicketSubjectLen+1)
	for i := range huge {
		huge[i] = 'x'
	}
	if _, err := a.store.CreateTicket(role.ID, submitter.ID, string(huge), "body"); err == nil {
		t.Error("an oversized subject should be rejected")
	}
	hugeBody := make([]byte, maxTicketBodyLen+1)
	for i := range hugeBody {
		hugeBody[i] = 'x'
	}
	if _, err := a.store.CreateTicket(role.ID, submitter.ID, "subject", string(hugeBody)); err == nil {
		t.Error("an oversized body should be rejected")
	}
}
