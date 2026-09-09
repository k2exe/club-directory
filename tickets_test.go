package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setupTicketFixture(t *testing.T, a *App) (role CustomRole, submitter, supporter1, supporter2, stranger *Member) {
	t.Helper()
	r, err := a.store.SaveCustomRole(CustomRole{Name: "Web Admin", Color: "#2f8f8f"})
	if err != nil {
		t.Fatal(err)
	}
	submitter = mustCreate(t, a, &Member{Email: "member@example.com", Name: "Member", CallSign: "W1MEM",
		Role: RoleMember, Status: StatusActive})
	supporter1 = mustCreate(t, a, &Member{Email: "webadmin@example.com", Name: "Webby", CallSign: "W1WEB",
		Role: RoleMember, Status: StatusActive})
	supporter2 = mustCreate(t, a, &Member{Email: "webadmin2@example.com", Name: "Webby Two", CallSign: "W2WEB",
		Role: RoleMember, Status: StatusActive})
	stranger = mustCreate(t, a, &Member{Email: "stranger@example.com", Name: "Stranger", CallSign: "W9STR",
		Role: RoleMember, Status: StatusActive})
	if err := a.store.SetMemberRoles(supporter1.ID, []string{r.ID}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetMemberRoles(supporter2.ID, []string{r.ID}); err != nil {
		t.Fatal(err)
	}
	return r, submitter, supporter1, supporter2, stranger
}

// Regression: CreateTicket used to call s.ByID (which acquires the store's
// sole SQLite connection) while already holding that same connection via an
// open transaction, deadlocking forever. Bound the call so a regression
// fails the test instead of hanging the whole suite.
func TestRegressionCreateTicketDoesNotDeadlock(t *testing.T) {
	a := testApp(t)
	_, submitter, _, _, _ := setupTicketFixture(t, a)
	role := a.store.CustomRoles()[0]

	done := make(chan error, 1)
	go func() {
		_, err := a.store.CreateTicket(role.ID, submitter.ID, "Subject", "Body")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CreateTicket did not return within 3s — likely deadlocked on the single SQLite connection")
	}
}

// Regression: a supporter whose account has since gone inactive (banned,
// suspended, left) keeps the role assignment — nothing revokes it on a
// status change — but must lose queue visibility and stop being emailed.
// The submitter's own access to their own ticket is a separate rule and
// must NOT be affected by their status.
func TestRegressionInactiveSupporterLosesTicketAccess(t *testing.T) {
	a := testApp(t)
	role, submitter, supporter1, _, _ := setupTicketFixture(t, a)
	ticket, err := a.store.CreateTicket(role.ID, submitter.ID, "Help", "Something broke")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.store.Update(supporter1.ID, func(x *Member) error { x.Status = StatusFormer; return nil }); err != nil {
		t.Fatal(err)
	}
	supporter1, _ = a.store.ByID(supporter1.ID)
	if a.canAccessTicket(supporter1, ticket) {
		t.Error("a former supporter should lose ticket-queue access")
	}
	if got := a.store.MembersWithRole(role.ID); len(got) != 1 {
		t.Errorf("MembersWithRole should drop the former supporter, got %d eligible", len(got))
	}

	if _, err := a.store.Update(submitter.ID, func(x *Member) error { x.Status = StatusFormer; return nil }); err != nil {
		t.Fatal(err)
	}
	submitter, _ = a.store.ByID(submitter.ID)
	if !a.canAccessTicket(submitter, ticket) {
		t.Error("a former member must still be able to see their own ticket")
	}
}

// A pending or otherwise low-trust account can open tickets, but not an
// unlimited number of them — each one fans out email to every supporter.
func TestTicketCreationIsRateLimited(t *testing.T) {
	a := testApp(t)
	a.ticketRL = newLimiter(3, time.Hour)
	role, submitter, _, _, _ := setupTicketFixture(t, a)

	for i := 0; i < 3; i++ {
		form := url.Values{"role_id": {role.ID}, "subject": {"Issue"}, "body": {"details"}}
		if code := a.post(t, "/tickets", form, submitter).Code; code != http.StatusSeeOther {
			t.Fatalf("ticket %d: got %d, want redirect", i, code)
		}
	}
	form := url.Values{"role_id": {role.ID}, "subject": {"One too many"}, "body": {"details"}}
	a.post(t, "/tickets", form, submitter)
	for _, tk := range a.store.TicketsForMember(submitter.ID) {
		if tk.Subject == "One too many" {
			t.Error("the 4th ticket within the rate-limit window should have been refused, not created")
		}
	}
}

func TestTicketAccessControl(t *testing.T) {
	a := testApp(t)
	role, submitter, supporter1, _, stranger := setupTicketFixture(t, a)
	ticket, err := a.store.CreateTicket(role.ID, submitter.ID, "Help", "Something is broken")
	if err != nil {
		t.Fatal(err)
	}
	if code := a.get(t, "/tickets/"+ticket.ID, submitter).Code; code != http.StatusOK {
		t.Errorf("submitter: got %d, want 200", code)
	}
	if code := a.get(t, "/tickets/"+ticket.ID, supporter1).Code; code != http.StatusOK {
		t.Errorf("supporter: got %d, want 200", code)
	}
	if code := a.get(t, "/tickets/"+ticket.ID, stranger).Code; code == http.StatusOK {
		t.Error("an unrelated member should not be able to view the ticket")
	}
}

// Multiple members holding the same support role must ALL be notified —
// this is the specific requirement from the brief.
func TestTicketCreationNotifiesAllSupporters(t *testing.T) {
	a := testApp(t)
	role, submitter, _, _, _ := setupTicketFixture(t, a)

	form := url.Values{"role_id": {role.ID}, "subject": {"Photo issue"}, "body": {"My photo is not showing up."}}
	if code := a.post(t, "/tickets", form, submitter).Code; code != http.StatusSeeOther {
		t.Fatalf("ticket create returned %d", code)
	}
	if got := readOutboxFor(t, a, "webadmin_example.com"); len(got) == 0 {
		t.Error("supporter 1 should have been emailed about the new ticket")
	}
	if got := readOutboxFor(t, a, "webadmin2_example.com"); len(got) == 0 {
		t.Error("supporter 2 should ALSO have been emailed about the new ticket")
	}
}

// A reply notifies everyone attached to the ticket except whoever just acted.
func TestTicketReplyNotifiesEveryoneButActor(t *testing.T) {
	a := testApp(t)
	role, submitter, supporter1, _, _ := setupTicketFixture(t, a)
	ticket, err := a.store.CreateTicket(role.ID, submitter.ID, "Help", "It is broken")
	if err != nil {
		t.Fatal(err)
	}
	// Drain the ticket-created emails so the reply's emails are unambiguous.
	clearOutbox(t, a)

	form := url.Values{"body": {"Looking into it now."}, "status": {"waiting_user"}}
	if code := a.post(t, "/tickets/"+ticket.ID+"/reply", form, supporter1).Code; code != http.StatusSeeOther {
		t.Fatalf("reply returned %d", code)
	}
	if got := readOutboxFor(t, a, "member_example.com"); len(got) == 0 {
		t.Error("the submitter should be emailed about a supporter's reply")
	}
	if got := readOutboxFor(t, a, "webadmin2_example.com"); len(got) == 0 {
		t.Error("the OTHER supporter should also be emailed")
	}
	if got := readOutboxFor(t, a, "webadmin_example.com"); len(got) != 0 {
		t.Error("the supporter who just replied should not be emailed about their own reply")
	}
	updated, err := a.store.TicketByID(ticket.ID)
	if err != nil || updated.Status != TicketWaitingUser {
		t.Errorf("status = %v, %v; want waiting_user", updated.Status, err)
	}
}

func clearOutbox(t *testing.T, a *App) {
	t.Helper()
	for _, e := range outboxFiles(t, a) {
		_ = os.Remove(filepath.Join(a.store.OutboxDir(), e.Name()))
	}
}
