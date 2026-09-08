package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------- public nets page (any signed-in member) ----------

type netCard struct {
	*Net
	Subscribed bool
	SubCount   int
	Next       string
}

func (a *App) handleNets(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	subs := a.store.SubscribedNetIDs(me.ID)
	cards := []netCard{}
	for _, n := range a.store.Nets(true) {
		next := ""
		if t, ok := n.nextOccurrence(time.Now()); ok {
			next = t.Format("Mon 2 Jan, 15:04")
		}
		cards = append(cards, netCard{Net: n, Subscribed: subs[n.ID], SubCount: len(a.store.Subscribers(n.ID)), Next: next})
	}
	a.render(w, r, "nets", "Club nets", map[string]any{
		"Nets":       cards,
		"NetRemind":  me.NetRemind,
		"IsNetAdmin": me.StaffRole == StaffNetAdmin,
	})
}

func (a *App) handleNetSubscribe(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.fail(w, r, "Unknown net.", "/nets")
		return
	}
	if _, err := a.store.NetByID(id); err != nil {
		a.fail(w, r, "Unknown net.", "/nets")
		return
	}
	// Subscribing requires the global opt-in; the UI offers the toggle.
	if !me.NetRemind {
		a.fail(w, r, "Turn on net reminders in your details first (the NetRemind box), then subscribe.", "/nets")
		return
	}
	a.store.Subscribe(me.ID, id)
	a.audit.Write(me.Email, "netremind.subscribe", me.ID, fmt.Sprintf("net %d", id), a.clientIP(r))
	a.ok(w, r, "Subscribed — you will get an email reminder before this net.", "/nets")
}

func (a *App) handleNetUnsubscribe(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.fail(w, r, "Unknown net.", "/nets")
		return
	}
	a.store.Unsubscribe(me.ID, id)
	a.audit.Write(me.Email, "netremind.unsubscribe", me.ID, fmt.Sprintf("net %d", id), a.clientIP(r))
	a.ok(w, r, "Unsubscribed from that net.", "/nets")
}

// ---------- onboarding opt-in (part of the account form) ----------

// handleAccountSave already saves member details; the NetRemind checkbox
// rides along in the same form (see templates/pages/account.html).

// ---------- net admin scheduling ----------

func requireNetAdmin(a *App, h http.HandlerFunc) http.HandlerFunc {
	return a.requireMember(func(w http.ResponseWriter, r *http.Request) {
		me := a.current(r)
		if accessOf(me) != AccessAdmin && me.StaffRole != StaffNetAdmin {
			http.Error(w, "Net admins only.", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

type netFormData struct {
	Net *Net
}

func (a *App) handleNetNewForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "net_edit", "Schedule a net", netFormData{Net: &Net{Recur: RecurWeekly, Weekdays: []time.Weekday{time.Monday}, EveryNWeeks: 2, NthWeek: 1, LocalTime: "19:30", Active: true}})
}

func (a *App) handleNetEditForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	n, err := a.store.NetByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	a.render(w, r, "net_edit", "Edit net", netFormData{Net: n})
}

// parseNetForm reads the schedule form. Returns (net fields, error message).
func parseNetForm(r *http.Request) (*Net, string) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return nil, "Give the net a name."
	}
	recur := Recur(r.FormValue("recur"))
	if !validRecur(recur) {
		return nil, "Pick a schedule."
	}
	var weekdays []time.Weekday
	for _, d := range parseWeekdays(strings.Join(r.Form["weekday"], ",")) {
		weekdays = append(weekdays, d)
	}
	switch recur {
	case RecurWeekly, RecurBiweekly, RecurMonthly:
		if len(weekdays) == 0 {
			return nil, "Pick at least one day of the week."
		}
		if recur == RecurMonthly && len(weekdays) > 1 {
			weekdays = weekdays[:1] // monthly applies to a single weekday
		}
	case RecurDaily, RecurWeekdays, RecurWeekends:
		weekdays = nil // pattern implies the days
	}
	localTime := strings.TrimSpace(r.FormValue("local_time"))
	if _, err := time.Parse("15:04", localTime); err != nil {
		return nil, "Enter the start time as HH:MM (24-hour)."
	}
	n := &Net{
		Name:        name,
		Description: strings.TrimSpace(r.FormValue("description")),
		Recur:       recur,
		Weekdays:    weekdays,
		LocalTime:   localTime,
		Active:      r.FormValue("active") != "",
	}
	if v := r.FormValue("every_n_weeks"); v != "" {
		if n_, err := strconv.Atoi(v); err == nil && n_ >= 1 && n_ <= 12 {
			n.EveryNWeeks = n_
		} else {
			n.EveryNWeeks = 2
		}
	} else {
		n.EveryNWeeks = 2
	}
	if v := r.FormValue("nth_week"); v != "" {
		if n_, err := strconv.Atoi(v); err == nil && n_ >= 1 && n_ <= 5 {
			n.NthWeek = n_
		} else {
			n.NthWeek = 1
		}
	} else {
		n.NthWeek = 1
	}
	return n, ""
}

func (a *App) handleNetCreate(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	n, errMsg := parseNetForm(r)
	if errMsg != "" {
		a.fail(w, r, errMsg, "/nets/new")
		return
	}
	n.CreatedBy = me.ID
	created, err := a.store.CreateNet(n)
	if err != nil {
		a.fail(w, r, "Could not save the net.", "/nets/new")
		return
	}
	a.audit.Write(me.Email, "netremind.net_create", fmt.Sprintf("%d", created.ID), created.Name, a.clientIP(r))
	a.ok(w, r, fmt.Sprintf("Net scheduled: %s (%s).", created.Name, created.Describe()), "/nets")
}

func (a *App) handleNetSave(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	n, errMsg := parseNetForm(r)
	if errMsg != "" {
		a.fail(w, r, errMsg, fmt.Sprintf("/nets/%d/edit", id))
		return
	}
	_, err = a.store.UpdateNet(id, func(x *Net) error {
		x.Name = n.Name
		x.Description = n.Description
		x.Recur = n.Recur
		x.Weekdays = n.Weekdays
		x.EveryNWeeks = n.EveryNWeeks
		x.NthWeek = n.NthWeek
		x.LocalTime = n.LocalTime
		x.Active = n.Active
		return nil
	})
	if err != nil {
		a.fail(w, r, "Could not save the net.", fmt.Sprintf("/nets/%d/edit", id))
		return
	}
	a.audit.Write(me.Email, "netremind.net_edit", fmt.Sprintf("%d", id), n.Name, a.clientIP(r))
	a.ok(w, r, "Net updated.", "/nets")
}

func (a *App) handleNetDelete(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.store.DeleteNet(id); err != nil {
		a.fail(w, r, "Could not delete the net.", "/nets")
		return
	}
	a.audit.Write(me.Email, "netremind.net_delete", fmt.Sprintf("%d", id), "", a.clientIP(r))
	a.ok(w, r, "Net deleted.", "/nets")
}

func validRecur(r Recur) bool {
	for _, x := range allRecurs {
		if x == r {
			return true
		}
	}
	return false
}
