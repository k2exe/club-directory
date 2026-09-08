package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNetScheduling(t *testing.T) {
	a := testApp(t)
	admin := mustCreate(t, a, &Member{Email: "netadmin@t.local", Role: RoleAdmin, Status: StatusActive, StaffRole: StaffNetAdmin, TOTPEnabled: enrolled()})
	plain := mustCreate(t, a, &Member{Email: "plain@t.local", Name: "Plain", CallSign: "K1P", Status: StatusActive})

	// A plain member cannot reach the schedule form.
	if rec := a.get(t, "/nets/new", plain); rec.Code != 403 {
		t.Fatalf("plain member scheduling: got %d, want 403", rec.Code)
	}
	// A net admin can.
	if rec := a.get(t, "/nets/new", admin); rec.Code != 200 {
		t.Fatalf("net admin form: got %d", rec.Code)
	}

	// Create a weekly net (Mon) via the form.
	form := url.Values{
		"name": {"Tuesday Net"}, "recur": {"weekly"}, "weekday": {"Tue"},
		"local_time": {"19:30"}, "active": {"on"},
	}
	if rec := a.post(t, "/nets/new", form, admin); rec.Code != 303 {
		t.Fatalf("create net: got %d: %s", rec.Code, rec.Body.String())
	}
	nets := a.store.Nets(false)
	if len(nets) != 1 || nets[0].Name != "Tuesday Net" || len(nets[0].Weekdays) != 1 || nets[0].Weekdays[0] != time.Tuesday {
		t.Fatalf("net not persisted correctly: %+v", nets)
	}
	net := nets[0]

	// Occurrence computation: next occurrence after a Wednesday is next Tuesday.
	wed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local) // a Wednesday
	next, ok := net.nextOccurrence(wed)
	if !ok || next.Weekday() != time.Tuesday || next.Format("15:04") != "19:30" {
		t.Fatalf("next occurrence: got %v ok=%v", next, ok)
	}
	// ...and it should be 6 days ahead.
	if days := int(next.Sub(time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local)).Hours() / 24); days != 6 {
		t.Fatalf("expected +6 days, got %d", days)
	}

	// Subscription requires the NetRemind opt-in.
	if rec := a.post(t, "/nets/"+itoa64(net.ID)+"/subscribe", url.Values{}, plain); rec.Code != 303 {
		t.Fatalf("subscribe without opt-in: got %d", rec.Code)
	}
	if len(a.store.Subscribers(net.ID)) != 0 {
		t.Fatalf("subscribed without opt-in")
	}

	// Opt in, then subscribe.
	a.store.Update(plain.ID, func(x *Member) error { x.NetRemind = true; return nil })
	if rec := a.post(t, "/nets/"+itoa64(net.ID)+"/subscribe", url.Values{}, plain); rec.Code != 303 {
		t.Fatalf("subscribe with opt-in: got %d", rec.Code)
	}
	if got := a.store.Subscribers(net.ID); len(got) != 1 || got[0] != plain.ID {
		t.Fatalf("subscriber missing: %v", got)
	}

	// Reminders for the net's day reach the subscriber; other days don't.
	tue := time.Date(2026, 9, 8, 0, 30, 0, 0, time.Local) // a Tuesday
	if n := a.SendNetRemindersForDay(tue); n != 1 {
		t.Fatalf("reminders sent on net day: %d, want 1", n)
	}
	if n := a.SendNetRemindersForDay(wed); n != 0 {
		t.Fatalf("reminders sent on non-net day: %d, want 0", n)
	}
	mail := readOutboxMust(t, a)
	last := mail[len(mail)-1]
	if !strings.Contains(last, "plain@t.local") || !strings.Contains(last, "Tuesday Net") || !strings.Contains(last, "19:30") {
		t.Fatalf("reminder mail wrong: %.300s", last)
	}

	// Non-opted-in members never get mail even if subscribed by ID.
	a.store.Update(plain.ID, func(x *Member) error { x.NetRemind = false; return nil })
	if n := a.SendNetRemindersForDay(tue); n != 0 {
		t.Fatalf("reminder sent to non-opted-in member: %d", n)
	}

	// Editing: switch to every-2-weeks and verify the gap.
	a.store.Update(plain.ID, func(x *Member) error { x.NetRemind = true; return nil })
	edit := url.Values{
		"name": {"Tuesday Net"}, "recur": {"biweekly"}, "weekday": {"Tue"},
		"every_n_weeks": {"2"}, "local_time": {"20:00"}, "active": {"on"},
	}
	if rec := a.post(t, "/nets/"+itoa64(net.ID)+"/edit", edit, admin); rec.Code != 303 {
		t.Fatalf("edit net: got %d", rec.Code)
	}
	updated, _ := a.store.NetByID(net.ID)
	upc := updated.upcoming(time.Date(2026, 9, 8, 0, 0, 0, 0, time.Local), 3)
	if len(upc) != 3 {
		t.Fatalf("upcoming: got %d", len(upc))
	}
	if gap := int(upc[1].Sub(upc[0]).Hours() / 24); gap != 14 {
		t.Fatalf("biweekly gap: got %d days, want 14", gap)
	}

	// Deleting removes the net and its subscriptions.
	if rec := a.post(t, "/nets/"+itoa64(net.ID)+"/delete", url.Values{}, admin); rec.Code != 303 {
		t.Fatalf("delete net: got %d", rec.Code)
	}
	if _, err := a.store.NetByID(net.ID); err != ErrNotFound {
		t.Fatalf("net not deleted: %v", err)
	}
}

func TestRecurPatterns(t *testing.T) {
	base := &Net{Name: "x", LocalTime: "10:00", CreatedAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.Local)} // a Monday

	// daily: next day
	n := *base
	n.Recur = RecurDaily
	got, _ := n.nextOccurrence(time.Date(2026, 9, 8, 23, 0, 0, 0, time.Local))
	if got.Format("2006-01-02") != "2026-09-09" {
		t.Fatalf("daily: got %v", got)
	}

	// weekdays skips Sat/Sun
	n.Recur = RecurWeekdays
	got, _ = n.nextOccurrence(time.Date(2026, 9, 11, 12, 0, 0, 0, time.Local)) // Friday
	if got.Weekday() != time.Monday || got.Format("2006-01-02") != "2026-09-14" {
		t.Fatalf("weekdays: got %v", got)
	}

	// weekends
	n.Recur = RecurWeekends
	got, _ = n.nextOccurrence(time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)) // Wed
	if got.Weekday() != time.Saturday {
		t.Fatalf("weekends: got %v", got)
	}

	// monthly: first Tuesday — net created Mon 7 Sep 2026; first Tue of Oct is the 6th
	n.Recur = RecurMonthly
	n.Weekdays = []time.Weekday{time.Tuesday}
	n.NthWeek = 1
	got, _ = n.nextOccurrence(time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local))
	if got.Format("2006-01-02") != "2026-10-06" {
		t.Fatalf("monthly first Tuesday: got %v", got)
	}
	// last Friday of September 2026 is the 25th
	n2 := *base
	n2.Recur = RecurMonthly
	n2.Weekdays = []time.Weekday{time.Friday}
	n2.NthWeek = 5
	got, _ = n2.nextOccurrence(time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local))
	if got.Format("2006-01-02") != "2026-09-25" {
		t.Fatalf("monthly last Friday: got %v", got)
	}

	// biweekly every 3 weeks
	n3 := *base
	n3.Recur = RecurBiweekly
	n3.Weekdays = []time.Weekday{time.Wednesday}
	n3.EveryNWeeks = 3
	upc := n3.upcoming(time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local), 3)
	if len(upc) != 3 {
		t.Fatalf("biweekly upcoming: %d", len(upc))
	}
	if gap := int(upc[1].Sub(upc[0]).Hours() / 24); gap != 21 {
		t.Fatalf("3-week gap: got %d", gap)
	}
}

func TestReminderLoopFiresAtTime(t *testing.T) {
	a := testApp(t)
	admin := mustCreate(t, a, &Member{Email: "na@t.local", Name: "NA", CallSign: "K1NA", Status: StatusActive, StaffRole: StaffNetAdmin, NetRemind: true})
	// Schedule "now"-ish: pick a local time a minute in the future so the
	// daily net matches today but the occurrence is still ahead.
	future := time.Now().Add(2 * time.Minute)
	form := url.Values{
		"name": {"Daily Net"}, "recur": {"daily"},
		"local_time": {future.Format("15:04")}, "active": {"on"},
	}
	if rec := a.post(t, "/nets/new", form, admin); rec.Code != 303 {
		t.Fatalf("create: %d", rec.Code)
	}
	// The admin must also be subscribed to the net to get its reminder.
	nets := a.store.Nets(false)
	a.store.Subscribe(admin.ID, nets[0].ID)
	n := a.SendNetRemindersForDay(time.Now())
	if n != 1 {
		t.Fatalf("expected 1 reminder, got %d", n)
	}
	mail := readOutboxMust(t, a)
	if !strings.Contains(mail[len(mail)-1], "na@t.local") {
		t.Fatalf("reminder went to wrong address")
	}

	// todayAt helper sanity
	if !todayAt(8, 0).Equal(time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 8, 0, 0, 0, time.Local)) {
		t.Fatalf("todayAt wrong")
	}
}

func TestNetRemindOptInFlow(t *testing.T) {
	a := testApp(t)
	m := mustCreate(t, a, &Member{Email: "o@t.local", Name: "O", CallSign: "K9O", Status: StatusActive})

	// Opt in via the onboarding checkbox route.
	if rec := a.post(t, "/account/netremind", url.Values{"net_remind": {"on"}}, m); rec.Code != 303 {
		t.Fatalf("opt-in: got %d", rec.Code)
	}
	got, _ := a.store.ByID(m.ID)
	if !got.NetRemind {
		t.Fatalf("opt-in not persisted")
	}
	// Opt back out.
	if rec := a.post(t, "/account/netremind", url.Values{}, m); rec.Code != 303 {
		t.Fatalf("opt-out: got %d", rec.Code)
	}
	got, _ = a.store.ByID(m.ID)
	if got.NetRemind {
		t.Fatalf("opt-out not persisted")
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
