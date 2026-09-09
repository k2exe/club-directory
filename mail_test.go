package main

import (
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
// "hi\r\nX-Injected: yes" splits into two raw header lines.
func TestRegressionTicketSubjectCannotInjectHeaders(t *testing.T) {
	a := testApp(t)
	role, submitter, _, _, _ := setupTicketFixture(t, a)
	evilSubject := "Need help\r\nX-Injected: yes\r\nBcc: attacker@evil.test"

	if _, err := a.store.CreateTicket(role.ID, submitter.ID, evilSubject, "body"); err != nil {
		t.Fatal(err)
	}
	for _, e := range outboxFiles(t, a) {
		raw, err := os.ReadFile(filepath.Join(a.store.OutboxDir(), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		msg := string(raw)
		if strings.Contains(msg, "X-Injected:") || strings.Contains(msg, "Bcc:") {
			t.Fatalf("email header injection succeeded, raw message:\n%s", msg)
		}
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
