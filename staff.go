package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Staff responsibilities carry two notification duties: when someone joins
// the club, the Echolink Manager hears about it (they provision the Echolink
// roster); and any signed-in member can raise a support request that goes to
// whoever holds the relevant role.
//
// A member holding several roles gets one copy of each message; the request
// form picks a single role per request so it lands with one responsible
// person rather than being broadcast.

type supportTopic struct {
	Role  StaffRole
	Label string
}

var supportTopics = []supportTopic{
	{StaffWebsite, "Website"},
	{StaffEcholink, "Echolink"},
	{StaffArednMesh, "AREDN mesh"},
}

// notifyStaffNewMember emails the Echolink Manager whenever a member is
// added to the roster (admin add or approved self sign-up).
func (a *App) notifyStaffNewMember(m *Member) {
	holders := a.store.ByStaffRole(StaffEcholink)
	if len(holders) == 0 {
		return // no one holds the role; nothing to do
	}
	set := a.store.Settings()
	subject := "New member added to " + set.ClubName
	for _, h := range holders {
		if h.Email == m.Email {
			continue // don't email the new member about themselves
		}
		body := fmt.Sprintf(`Hello %s,

A new member was added to the %s roster:

  Call sign: %s
  Name:      %s
  Email:     %s
  Joined:    %s

You are listed as the club's Echolink Manager, so this is your cue to
provision them on the Echolink roster if applicable.

%s
`, h.Display(), set.ClubName,
			orDash(m.CallSign), orDash(m.Name), m.Email,
			m.JoinedAt.Format("2 Jan 2006"),
			set.ClubName)
		if err := a.mailer.Send(h.Email, subject, body); err != nil {
			// Mailer already spools on failure; just record it.
			a.audit.Write("system", "staff.notify_failed", h.ID, "new_member -> echolink", "")
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---------- support requests ----------

// staffTopicsForViewer marks which topics a viewer can pick: the ones they
// hold themselves are excluded — you don't request support from yourself.
func (a *App) supportTopicsForViewer(viewer *Member) []supportTopic {
	out := []supportTopic{}
	for _, t := range supportTopics {
		if viewer != nil && viewer.StaffRole == t.Role {
			continue
		}
		out = append(out, t)
	}
	return out
}

func (a *App) handleSupportForm(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	a.render(w, r, "support", "Request support", map[string]any{
		"Topics":     a.supportTopicsForViewer(me),
		"Preselect":  StaffRole(r.URL.Query().Get("topic")),
	})
}

func (a *App) handleSupportSubmit(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	topic := StaffRole(r.FormValue("topic"))
	if !validStaffRole(topic) {
		a.fail(w, r, "Pick what you need help with.", "/support")
		return
	}
	// The requester may only pick a topic they do not hold themselves.
	if me != nil && me.StaffRole == topic {
		a.fail(w, r, "You are the manager for that area — sort it out among yourselves!", "/support")
		return
	}
	holders := a.store.ByStaffRole(topic)
	if len(holders) == 0 {
		a.fail(w, r, "No one currently holds that role. Ask a club admin to assign it.", "/support")
		return
	}
	msg := strings.TrimSpace(r.FormValue("message"))
	if msg == "" {
		a.fail(w, r, "Describe what you need help with.", "/support")
		return
	}
	if len(msg) > 4000 {
		msg = msg[:4000]
	}
	set := a.store.Settings()
	subject := "[club-directory] Support request: " + topic.Label()
	for _, h := range holders {
		body := fmt.Sprintf(`Hello %s,

%s sent a support request for your area (%s):

%s

Reply to %s to help them.

You are receiving this because you hold the %s role in %s.
`, h.Display(), me.Display(), topic.Label(),
			indentQuote(msg),
			me.Email, topic.Label(), set.ClubName)
		if err := a.mailer.Send(h.Email, subject, body); err != nil {
			a.audit.Write("system", "staff.notify_failed", h.ID, "support -> "+string(topic), "")
		}
	}
	a.audit.Write(me.Email, "support.request", me.ID, string(topic), a.clientIP(r))
	a.ok(w, r, "Your request was sent to the "+topic.Label()+".", "/support")
}

func indentQuote(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// supportLinkFor renders the "Request support" button target with the
// caller's preferred topic preselected. Used by templates via SupportLink.
func supportLinkFor(topic StaffRole) string {
	v := url.Values{}
	if topic != StaffNone {
		v.Set("topic", string(topic))
	}
	if len(v) == 0 {
		return "/support"
	}
	return "/support?" + v.Encode()
}
