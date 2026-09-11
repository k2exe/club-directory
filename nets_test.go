package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func TestRecurrenceOnce(t *testing.T) {
	n := Net{Recur: RecurOnce, StartsAt: utc(2026, 9, 20, 14, 0)}
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 1, 0, 0))
	if !ok || !occ.Equal(n.StartsAt) {
		t.Fatalf("got %v, %v; want %v, true", occ, ok, n.StartsAt)
	}
	if _, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 21, 0, 0)); ok {
		t.Error("a one-off net whose time has passed should have no next occurrence")
	}
}

func TestRecurrenceDaily(t *testing.T) {
	n := Net{Recur: RecurDaily, StartsAt: utc(2026, 9, 1, 19, 0)}
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 8, 20, 0)) // just past today's 19:00
	if !ok || !occ.Equal(utc(2026, 9, 9, 19, 0)) {
		t.Fatalf("got %v, %v; want 2026-09-09 19:00", occ, ok)
	}
	occ, ok = nextOccurrenceOnOrAfter(n, utc(2026, 9, 8, 18, 0)) // before today's 19:00
	if !ok || !occ.Equal(utc(2026, 9, 8, 19, 0)) {
		t.Fatalf("got %v, %v; want 2026-09-08 19:00", occ, ok)
	}
}

func TestRecurrenceEveryOtherDay(t *testing.T) {
	n := Net{Recur: RecurEveryOther, StartsAt: utc(2026, 9, 1, 19, 0)}
	// Sept 1, 3, 5, 7, 9 ... — asking as-of the 8th should land on the 9th.
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 8, 0, 0))
	if !ok || !occ.Equal(utc(2026, 9, 9, 19, 0)) {
		t.Fatalf("got %v, %v; want 2026-09-09 19:00", occ, ok)
	}
}

func TestRecurrenceWeeklyCustomDays(t *testing.T) {
	// Tue + Thu, reference start on a Tuesday.
	n := Net{Recur: RecurWeekly, StartsAt: utc(2026, 9, 1, 19, 0),
		WeekdaysMask: weekdayBit(time.Tuesday) | weekdayBit(time.Thursday)}
	// Sept 1 2026 is a Tuesday (verified via `date -d 2026-09-01`).
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 2, 0, 0)) // day after Tuesday
	if !ok || !occ.Equal(utc(2026, 9, 3, 19, 0)) {               // the following Thursday
		t.Fatalf("got %v, %v; want 2026-09-03 19:00 (Thursday)", occ, ok)
	}
}

func TestRecurrenceWeeklyPresets(t *testing.T) {
	weekdaysNet := Net{Recur: RecurWeekly, StartsAt: utc(2026, 9, 1, 19, 0), WeekdaysMask: weekdaysMask}
	// Sept 5 2026 is a Saturday; the next weekday occurrence is Monday the 7th.
	occ, ok := nextOccurrenceOnOrAfter(weekdaysNet, utc(2026, 9, 5, 0, 0))
	if !ok || occ.Weekday() != time.Monday || !occ.Equal(utc(2026, 9, 7, 19, 0)) {
		t.Fatalf("weekdays preset: got %v, %v; want Monday 2026-09-07", occ, ok)
	}

	weekendsNet := Net{Recur: RecurWeekly, StartsAt: utc(2026, 9, 1, 19, 0), WeekdaysMask: weekendsMask}
	occ, ok = nextOccurrenceOnOrAfter(weekendsNet, utc(2026, 9, 3, 0, 0)) // a Thursday
	if !ok || !occ.Equal(utc(2026, 9, 5, 19, 0)) {                        // the following Saturday
		t.Fatalf("weekends preset: got %v, %v; want Saturday 2026-09-05", occ, ok)
	}
}

func TestRecurrenceMonthlyDate(t *testing.T) {
	n := Net{Recur: RecurMonthlyDate, StartsAt: utc(2026, 1, 31, 18, 0)}
	// February 2027 has 28 days: the 31st clamps to the last real day.
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2027, 2, 1, 0, 0))
	if !ok || !occ.Equal(utc(2027, 2, 28, 18, 0)) {
		t.Fatalf("got %v, %v; want 2027-02-28 (clamped from the 31st)", occ, ok)
	}
}

// The example straight from the brief: "our net is the 3rd Wednesday of every month."
func TestRecurrenceMonthlyNthWeekday_3rdWednesday(t *testing.T) {
	n := Net{Recur: RecurMonthlyNth, StartsAt: utc(2026, 1, 1, 19, 0), NthWeek: 3, NthWeekday: time.Wednesday}
	// September 2026's Wednesdays are 2, 9, 16, 23, 30 (verified independently
	// via `date -d`) — the 3rd one is the 16th.
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 8, 0, 0))
	if !ok || !occ.Equal(utc(2026, 9, 16, 19, 0)) {
		t.Fatalf("got %v, %v; want 2026-09-16 19:00", occ, ok)
	}
}

func TestRecurrenceMonthlyNthWeekday_LastFriday(t *testing.T) {
	n := Net{Recur: RecurMonthlyNth, StartsAt: utc(2026, 1, 1, 19, 0), NthWeek: 5, NthWeekday: time.Friday}
	// October 2026's Fridays are 2, 9, 16, 23, 30 — "last" is the 30th.
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2026, 10, 1, 0, 0))
	if !ok || !occ.Equal(utc(2026, 10, 30, 19, 0)) {
		t.Fatalf("got %v, %v; want 2026-10-30 19:00", occ, ok)
	}
}

// Regression: an end date typed as "Sept 10" was parsed as 2026-09-10
// 00:00 UTC and then excluded any occurrence *on* that date, since a net at
// 19:00 is after midnight. "Stop repeating after Sept 10" should still run
// on the 10th.
func TestRegressionEndDateIsInclusive(t *testing.T) {
	end := time.Date(2026, 9, 10, 23, 59, 59, 0, time.UTC)
	n := Net{Recur: RecurDaily, StartsAt: utc(2026, 9, 1, 19, 0), EndsAt: &end}
	occ, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 10, 0, 0))
	if !ok || !occ.Equal(utc(2026, 9, 10, 19, 0)) {
		t.Fatalf("got %v, %v; want 2026-09-10 19:00 to still be included", occ, ok)
	}
	if _, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 11, 0, 0)); ok {
		t.Error("the 11th should be excluded — that's genuinely after the end date")
	}
}

func TestNetFormRejectsUnknownRecurKind(t *testing.T) {
	a := testApp(t)
	admin := mustCreate(t, a, &Member{Email: "boss@example.com", Name: "Boss", CallSign: "W1BOSS",
		Role: RoleAdmin, Status: StatusActive, TOTPEnabled: enrolled(), TOTPSecret: newTOTPSecret()})
	form := url.Values{
		"name": {"Sneaky Net"}, "starts_at": {"2026-09-20T14:00"}, "recur_kind": {"every_hour_on_the_hour"},
	}
	if code := a.post(t, "/nets", form, admin).Code; code != http.StatusSeeOther {
		t.Fatalf("post returned %d", code)
	}
	for _, n := range a.store.Nets() {
		if n.Name == "Sneaky Net" {
			t.Fatal("a net with an unrecognized recurrence kind should have been rejected, not saved")
		}
	}
}

func TestSaveNetRejectsInvalidRecurrenceInvariants(t *testing.T) {
	a := testApp(t)
	cases := []struct {
		name string
		n    Net
	}{
		{"unknown recur kind", Net{Name: "X", StartsAt: time.Now(), Recur: RecurKind("orbital")}},
		{"nth_week too low", Net{Name: "X", StartsAt: time.Now(), Recur: RecurMonthlyNth, NthWeek: 0, NthWeekday: time.Monday}},
		{"nth_week too high", Net{Name: "X", StartsAt: time.Now(), Recur: RecurMonthlyNth, NthWeek: 6, NthWeekday: time.Monday}},
		{"nth_weekday out of range", Net{Name: "X", StartsAt: time.Now(), Recur: RecurMonthlyNth, NthWeek: 2, NthWeekday: time.Weekday(99)}},
		{"weekday mask has invalid bits", Net{Name: "X", StartsAt: time.Now(), Recur: RecurWeekly, WeekdaysMask: 1 << 10}},
		{"end before start", func() Net {
			start := time.Now()
			end := start.Add(-24 * time.Hour)
			return Net{Name: "X", StartsAt: start, Recur: RecurDaily, EndsAt: &end}
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := a.store.SaveNet(c.n); err == nil {
				t.Error("expected SaveNet to reject this, got nil error")
			}
		})
	}
}

func TestRecurrenceRespectsEndsAt(t *testing.T) {
	end := utc(2026, 9, 10, 0, 0)
	n := Net{Recur: RecurDaily, StartsAt: utc(2026, 9, 1, 19, 0), EndsAt: &end}
	if _, ok := nextOccurrenceOnOrAfter(n, utc(2026, 9, 20, 0, 0)); ok {
		t.Error("a recurrence past its end date should report no next occurrence")
	}
}

// ---------- reminder scheduler ----------

func TestCheckRemindersFiresOnceAndDedups(t *testing.T) {
	a := testApp(t)
	member := mustCreate(t, a, &Member{Email: "reminded@example.com", Name: "R", CallSign: "W1RMD",
		Role: RoleMember, Status: StatusActive, NetRemindOptIn: true, NetRemindLeadMinutes: 15})
	starts := time.Now().Add(20 * time.Minute)
	n, err := a.store.SaveNet(Net{Name: "Test Net", Frequency: "146.850", Recur: RecurOnce, StartsAt: starts})
	if err != nil {
		t.Fatal(err)
	}
	due := starts.Add(-15 * time.Minute)

	a.checkReminders(due)
	files := outboxFiles(t, a)
	if len(files) != 1 {
		t.Fatalf("expected 1 reminder email, got %d", len(files))
	}

	// A second tick at the same instant (or a slightly later one within the
	// check window) must not send a duplicate.
	a.checkReminders(due.Add(10 * time.Second))
	if files := outboxFiles(t, a); len(files) != 1 {
		t.Errorf("expected dedup to prevent a second send, got %d emails", len(files))
	}
	_ = member
	_ = n
}

// Regression: sendNetReminder used to record a reminder as sent BEFORE
// attempting delivery, so a single SMTP failure permanently lost it — no
// later tick would ever retry, because presence in reminder_log is what
// stops retries. A failed send must leave the reminder unrecorded so a
// later tick (still within the catch-up window) retries and succeeds.
// Regression (the exact scenario from review): Mailer.Send used to return
// the SMTP error even when its own fallback spool succeeded, so
// sendNetReminder treated a durably-spooled message as "not captured
// anywhere" and released the claim for retry. Across a continuous outage
// spanning the full 15-minute catch-up window, that meant a fresh spooled
// copy — no longer silently overwritten, now that spool uses O_EXCL —
// every single tick: 16 duplicate files for one reminder. A message that
// is durably captured (delivered OR spooled) must be marked sent
// immediately and never retried, regardless of whether SMTP itself is up.
func TestRegressionSMTPOutageDoesNotDuplicateSpooledReminders(t *testing.T) {
	a := testApp(t)
	member := mustCreate(t, a, &Member{Email: "reminded@example.com", Name: "R", CallSign: "W1RMD",
		Role: RoleMember, Status: StatusActive, NetRemindOptIn: true, NetRemindLeadMinutes: 15})
	starts := time.Now().Add(20 * time.Minute)
	if _, err := a.store.SaveNet(Net{Name: "Test Net", Frequency: "146.850", Recur: RecurOnce, StartsAt: starts}); err != nil {
		t.Fatal(err)
	}
	due := starts.Add(-15 * time.Minute)

	// Point the mailer at a port nothing is listening on so SMTP delivery
	// fails, for the entire catch-up window — but the outbox (testApp's
	// TempDir) stays writable throughout, so every attempt's fallback
	// spool succeeds.
	a.mailer.Host, a.mailer.Port = "127.0.0.1", 1
	a.mailer.DialTimeout = 200 * time.Millisecond

	// The scheduler ticks once a minute; simulate the full 16-minute span
	// (due, plus the 15-minute catch-up window) the review reproduced.
	for m := 0; m <= 15; m++ {
		a.checkReminders(due.Add(time.Duration(m) * time.Minute))
	}

	sent, err := a.store.reminderIsSent(a.store.Nets()[0].ID, starts, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sent {
		t.Error("a message that was durably spooled should be marked sent on the very first attempt")
	}
	if files := outboxFiles(t, a); len(files) != 1 {
		t.Errorf("got %d outbox files across a 16-minute simulated outage, want exactly 1", len(files))
	}
}

// The other side of the same fix: if BOTH SMTP delivery and the fallback
// spool fail, the message genuinely isn't captured anywhere, and the claim
// must still be released so a later tick retries — this is the one case
// Mailer.Send is still allowed to return an error for.
func TestRegressionTotalFailureStillReleasesClaimForRetry(t *testing.T) {
	a := testApp(t)
	member := mustCreate(t, a, &Member{Email: "reminded@example.com", Name: "R", CallSign: "W1RMD",
		Role: RoleMember, Status: StatusActive, NetRemindOptIn: true, NetRemindLeadMinutes: 15})
	starts := time.Now().Add(20 * time.Minute)
	if _, err := a.store.SaveNet(Net{Name: "Test Net", Frequency: "146.850", Recur: RecurOnce, StartsAt: starts}); err != nil {
		t.Fatal(err)
	}
	due := starts.Add(-15 * time.Minute)

	a.mailer.Host, a.mailer.Port = "127.0.0.1", 1
	a.mailer.DialTimeout = 200 * time.Millisecond
	// A file where the outbox directory should be makes every spool
	// attempt fail too (os.OpenFile can't create a file inside a file).
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.mailer.Outbox = filepath.Join(blocked, "outbox")

	a.checkReminders(due)
	if sent, err := a.store.reminderIsSent(a.store.Nets()[0].ID, starts, member.ID); err != nil {
		t.Fatal(err)
	} else if sent {
		t.Fatal("a reminder that was captured nowhere must not be recorded as sent")
	}

	// "Everything recovers" — a later tick, still inside the catch-up
	// window, must retry rather than having given up after one failure.
	a.mailer.Host = ""
	a.mailer.Outbox = a.store.OutboxDir()
	a.checkReminders(due.Add(2 * time.Minute))
	if sent, err := a.store.reminderIsSent(a.store.Nets()[0].ID, starts, member.ID); err != nil {
		t.Fatal(err)
	} else if !sent {
		t.Error("the retried reminder should now be recorded as sent")
	}
}

// The claim itself must be atomic: concurrent callers racing for the same
// (net, occurrence, member) tuple must have exactly one winner, never zero
// and never more than one — this is the actual guarantee behind the
// at-least-once documentation on sendNetReminder.
func TestClaimReminderIsAtomicUnderConcurrency(t *testing.T) {
	a := testApp(t)
	occ := time.Now().Add(time.Hour)
	const n = 50
	results := make(chan bool, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := a.store.claimReminder("net1", occ, "member1")
			if err != nil {
				t.Error(err)
				return
			}
			results <- claimed
		}()
	}
	wg.Wait()
	close(results)
	claims := 0
	for c := range results {
		if c {
			claims++
		}
	}
	if claims != 1 {
		t.Errorf("got %d successful claims out of %d concurrent attempts, want exactly 1", claims, n)
	}
}

// A claim left "pending" by a process that crashed before releasing or
// marking it sent must eventually become reclaimable — otherwise a single
// crash mid-send permanently blocks that reminder, same class of bug as
// the original lost-reminder issue. But it must NOT be reclaimable while
// still fresh, or two processes really could send the same reminder twice
// well within a normal retry window.
func TestClaimReminderReclaimsOnlyAfterGoingStale(t *testing.T) {
	a := testApp(t)
	occ := time.Now().Add(time.Hour)
	if _, claimed, err := a.store.claimReminder("net1", occ, "member1"); err != nil || !claimed {
		t.Fatalf("initial claim: claimed=%v err=%v", claimed, err)
	}
	// Simulate the claim having crashed (never released or marked sent) a
	// while ago by backdating it directly.
	old := time.Now().Add(-reminderClaimStaleAfter - time.Minute)
	if _, err := a.store.db.Exec(`UPDATE reminder_log SET claimed_at = ? WHERE net_id = 'net1'`, formatTime(old)); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := a.store.claimReminder("net1", occ, "member1"); err != nil || !claimed {
		t.Fatalf("stale reclaim: claimed=%v err=%v, want true", claimed, err)
	}

	// A claim from a moment ago (not stale) must not be reclaimable.
	if _, claimed, err := a.store.claimReminder("net2", occ, "member1"); err != nil || !claimed {
		t.Fatalf("fresh claim on net2: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := a.store.claimReminder("net2", occ, "member1"); err != nil || claimed {
		t.Errorf("reclaiming a fresh (non-stale) pending claim should fail, got claimed=%v err=%v", claimed, err)
	}
}

// Regression: the exact scenario from review — worker A's claim goes stale,
// worker B reclaims it, then A (unaware it lost ownership) finally gets
// around to finishing its send and tries to complete or release "its"
// claim. Without an ownership token, A's call would silently act on B's
// claim: marking B's still-in-progress send as done prematurely, or
// deleting it out from under B. With the token, A's stale claimID no
// longer matches the row, so both calls become no-ops.
func TestRegressionStaleClaimCannotClobberNewOwner(t *testing.T) {
	a := testApp(t)
	occ := time.Now().Add(time.Hour)

	claimA, ok, err := a.store.claimReminder("net1", occ, "member1")
	if err != nil || !ok {
		t.Fatalf("worker A's claim: ok=%v err=%v", ok, err)
	}
	old := time.Now().Add(-reminderClaimStaleAfter - time.Minute)
	if _, err := a.store.db.Exec(`UPDATE reminder_log SET claimed_at = ? WHERE net_id = 'net1'`, formatTime(old)); err != nil {
		t.Fatal(err)
	}
	claimB, ok, err := a.store.claimReminder("net1", occ, "member1")
	if err != nil || !ok {
		t.Fatalf("worker B's reclaim: ok=%v err=%v", ok, err)
	}
	if claimA == claimB {
		t.Fatal("test setup broken: A and B ended up with the same claim ID")
	}

	// A, unaware it lost the claim, tries to release it (simulating its
	// delayed send having failed) — must not touch B's now-current claim.
	if err := a.store.releaseReminderClaim("net1", occ, "member1", claimA); err != nil {
		t.Fatal(err)
	}
	if sent, err := a.store.reminderIsSent("net1", occ, "member1"); err != nil {
		t.Fatal(err)
	} else if sent {
		t.Fatal("should not be marked sent yet")
	}
	var stillPending int
	a.store.db.QueryRow(`SELECT COUNT(*) FROM reminder_log WHERE net_id = 'net1' AND claim_id = ?`, claimB).Scan(&stillPending)
	if stillPending != 1 {
		t.Fatal("worker A's release (using its stale claim ID) deleted worker B's current claim")
	}

	// A also tries to mark its (stale) claim sent — must not mark B's
	// still-pending claim as sent.
	if err := a.store.markReminderSent("net1", occ, "member1", claimA); err != nil {
		t.Fatal(err)
	}
	if sent, err := a.store.reminderIsSent("net1", occ, "member1"); err != nil {
		t.Fatal(err)
	} else if sent {
		t.Fatal("worker A's mark-sent (using its stale claim ID) marked worker B's claim as sent")
	}

	// B, the actual current owner, can still legitimately complete it.
	if err := a.store.markReminderSent("net1", occ, "member1", claimB); err != nil {
		t.Fatal(err)
	}
	if sent, err := a.store.reminderIsSent("net1", occ, "member1"); err != nil || !sent {
		t.Fatalf("B's own mark-sent should have succeeded: sent=%v err=%v", sent, err)
	}
}

func TestCheckRemindersRespectsOptOutAndAccess(t *testing.T) {
	a := testApp(t)
	mustCreate(t, a, &Member{Email: "optedout@example.com", Name: "O", CallSign: "W1OUT",
		Role: RoleMember, Status: StatusActive, NetRemindOptIn: false, NetRemindLeadMinutes: 15})
	mustCreate(t, a, &Member{Email: "pending@example.com", Name: "P", CallSign: "W1PEN",
		Role: RoleMember, Status: StatusPending, NetRemindOptIn: true, NetRemindLeadMinutes: 15})
	starts := time.Now().Add(20 * time.Minute)
	if _, err := a.store.SaveNet(Net{Name: "Test Net", Frequency: "146.850", Recur: RecurOnce, StartsAt: starts}); err != nil {
		t.Fatal(err)
	}
	a.checkReminders(starts.Add(-15 * time.Minute))
	if files := outboxFiles(t, a); len(files) != 0 {
		t.Errorf("expected no reminders (opted out, or pending with no roster sight), got %d", len(files))
	}
}

func outboxFiles(t *testing.T, a *App) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(a.store.OutboxDir())
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// ---------- net-admin capability access ----------

func TestRequireNetAdminAccess(t *testing.T) {
	a := testApp(t)
	admin := mustCreate(t, a, &Member{Email: "boss@example.com", Name: "Boss", CallSign: "W1BOSS",
		Role: RoleAdmin, Status: StatusActive, TOTPEnabled: enrolled(), TOTPSecret: newTOTPSecret()})
	plain := mustCreate(t, a, &Member{Email: "plain@example.com", Name: "Plain", CallSign: "W1PLN",
		Role: RoleMember, Status: StatusActive})
	netAdmin := mustCreate(t, a, &Member{Email: "netops@example.com", Name: "Ops", CallSign: "W1OPS",
		Role: RoleMember, Status: StatusActive})

	role, err := a.store.SaveCustomRole(CustomRole{Name: "Net Admin", Color: "#7d5ba6", GrantsNetAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetMemberRoles(netAdmin.ID, []string{role.ID}); err != nil {
		t.Fatal(err)
	}

	if code := a.get(t, "/nets/new", admin).Code; code != 200 {
		t.Errorf("full admin: /nets/new = %d, want 200", code)
	}
	if code := a.get(t, "/nets/new", plain).Code; code != 403 {
		t.Errorf("plain member: /nets/new = %d, want 403", code)
	}
	if code := a.get(t, "/nets/new", netAdmin).Code; code != 200 {
		t.Errorf("net-admin role holder: /nets/new = %d, want 200", code)
	}
	if code := a.get(t, "/admin", netAdmin).Code; code != 403 {
		t.Errorf("net-admin role holder should NOT get full admin sight: /admin = %d, want 403", code)
	}
}

// Regression: a role assignment survives a status change (nothing revokes
// it), but the capability it grants must not. A former member holding a
// net-admin role used to get a 200 from /nets/new.
func TestRegressionInactiveRoleHolderLosesNetAdminAccess(t *testing.T) {
	a := testApp(t)
	role, err := a.store.SaveCustomRole(CustomRole{Name: "Net Admin", Color: "#7d5ba6", GrantsNetAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []Status{StatusFormer, StatusPending, StatusBanned, StatusSK} {
		m := mustCreate(t, a, &Member{Email: string(status) + "@example.com", Name: "X",
			CallSign: "W1" + strings.ToUpper(string(status)), Role: RoleMember, Status: status})
		if err := a.store.SetMemberRoles(m.ID, []string{role.ID}); err != nil {
			t.Fatal(err)
		}
		if a.store.GrantsNetAdmin(m.ID) {
			t.Errorf("%s holding a net-admin role should not grant it while inactive", status)
		}
		if code := a.get(t, "/nets/new", m).Code; code == http.StatusOK {
			t.Errorf("%s: /nets/new = 200, want a non-200 (403, or a redirect for banned/SK)", status)
		}
	}
}
