package main

import (
	"os"
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
