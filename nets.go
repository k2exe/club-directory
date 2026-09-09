package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// ---------- repeaters ----------

type Repeater struct {
	ID        string
	Name      string
	Frequency string
	Notes     string
}

func (s *Store) Repeaters() []Repeater {
	rows, err := s.db.Query(`SELECT id, name, frequency, notes FROM repeaters ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Repeater{}
	for rows.Next() {
		var r Repeater
		if rows.Scan(&r.ID, &r.Name, &r.Frequency, &r.Notes) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) RepeaterByID(id string) (Repeater, error) {
	var r Repeater
	err := s.db.QueryRow(`SELECT id, name, frequency, notes FROM repeaters WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Frequency, &r.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		return Repeater{}, ErrNotFound
	}
	return r, err
}

func (s *Store) SaveRepeater(r Repeater) (Repeater, error) {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return Repeater{}, fmt.Errorf("a repeater needs a name")
	}
	if r.ID == "" {
		r.ID = newID()
		_, err := s.db.Exec(`INSERT INTO repeaters (id, name, frequency, notes) VALUES (?, ?, ?, ?)`,
			r.ID, r.Name, r.Frequency, r.Notes)
		return r, err
	}
	_, err := s.db.Exec(`UPDATE repeaters SET name = ?, frequency = ?, notes = ? WHERE id = ?`,
		r.Name, r.Frequency, r.Notes, r.ID)
	return r, err
}

func (s *Store) DeleteRepeater(id string) error {
	res, err := s.db.Exec("DELETE FROM repeaters WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- nets ----------

type RecurKind string

const (
	RecurOnce        RecurKind = "once"
	RecurDaily       RecurKind = "daily"
	RecurEveryOther  RecurKind = "every_other_day"
	RecurWeekly      RecurKind = "weekly"
	RecurMonthlyDate RecurKind = "monthly_date"
	RecurMonthlyNth  RecurKind = "monthly_nth_weekday"
)

var allRecurKinds = []RecurKind{RecurOnce, RecurDaily, RecurEveryOther, RecurWeekly, RecurMonthlyDate, RecurMonthlyNth}

func (k RecurKind) Label() string {
	switch k {
	case RecurOnce:
		return "One time only (special event)"
	case RecurDaily:
		return "Every day"
	case RecurEveryOther:
		return "Every other day"
	case RecurWeekly:
		return "Weekly, on selected day(s)"
	case RecurMonthlyDate:
		return "Monthly, same date"
	case RecurMonthlyNth:
		return "Monthly, on the Nth weekday"
	}
	return string(k)
}

// weekdayBit turns a time.Weekday into its bit in the mask (Sunday = bit 0).
func weekdayBit(d time.Weekday) int { return 1 << uint(d) }

const (
	weekdaysMask = 1<<time.Monday | 1<<time.Tuesday | 1<<time.Wednesday | 1<<time.Thursday | 1<<time.Friday
	weekendsMask = 1<<time.Saturday | 1<<time.Sunday
	allDaysMask  = weekdaysMask | weekendsMask
)

type Net struct {
	ID           string
	Name         string
	Frequency    string
	RepeaterID   string // "" = none
	Mode         string
	StartsAt     time.Time
	Recur        RecurKind
	WeekdaysMask int // for RecurWeekly
	NthWeek      int // 1-5 (5 = last), for RecurMonthlyNth
	NthWeekday   time.Weekday
	EndsAt       *time.Time
	CreatedBy    string
	CreatedAt    time.Time
}

// RecurDescription renders the recurrence rule in plain English for display.
func (n Net) RecurDescription() string {
	switch n.Recur {
	case RecurOnce:
		return "one time only"
	case RecurDaily:
		return "every day"
	case RecurEveryOther:
		return "every other day"
	case RecurWeekly:
		switch n.WeekdaysMask {
		case weekdaysMask:
			return "every weekday"
		case weekendsMask:
			return "weekends"
		case allDaysMask:
			return "every day"
		}
		var days []string
		for d := time.Sunday; d <= time.Saturday; d++ {
			if n.WeekdaysMask&weekdayBit(d) != 0 {
				days = append(days, d.String())
			}
		}
		if len(days) == 0 {
			return "weekly"
		}
		return "weekly on " + strings.Join(days, ", ")
	case RecurMonthlyDate:
		return fmt.Sprintf("monthly on the %s", ordinal(n.StartsAt.Day()))
	case RecurMonthlyNth:
		week := ordinal(n.NthWeek)
		if n.NthWeek == 5 {
			week = "last"
		}
		return fmt.Sprintf("the %s %s of every month", week, n.NthWeekday.String())
	}
	return string(n.Recur)
}

func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return fmt.Sprintf("%dth", n)
	}
	switch n % 10 {
	case 1:
		return fmt.Sprintf("%dst", n)
	case 2:
		return fmt.Sprintf("%dnd", n)
	case 3:
		return fmt.Sprintf("%drd", n)
	}
	return fmt.Sprintf("%dth", n)
}

func (s *Store) Nets() []Net {
	rows, err := s.db.Query(netColumns + " FROM nets ORDER BY starts_at")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Net{}
	for rows.Next() {
		n, err := scanNet(rows)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) NetByID(id string) (Net, error) {
	row := s.db.QueryRow(netColumns+" FROM nets WHERE id = ?", id)
	n, err := scanNet(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Net{}, ErrNotFound
	}
	return n, err
}

const netColumns = `SELECT id, name, frequency, repeater_id, mode, starts_at, recur_kind,
	recur_weekdays_mask, recur_nth_week, recur_nth_weekday, ends_at, created_by, created_at`

func scanNet(row scanner) (Net, error) {
	var n Net
	var repeaterID, endsAt sql.NullString
	var startsAt, createdAt string
	var nthWeekday int
	err := row.Scan(&n.ID, &n.Name, &n.Frequency, &repeaterID, &n.Mode, &startsAt, &n.Recur,
		&n.WeekdaysMask, &n.NthWeek, &nthWeekday, &endsAt, &n.CreatedBy, &createdAt)
	if err != nil {
		return Net{}, err
	}
	n.RepeaterID = repeaterID.String
	n.StartsAt = parseTime(startsAt)
	n.CreatedAt = parseTime(createdAt)
	n.NthWeekday = time.Weekday(nthWeekday)
	if endsAt.Valid {
		t := parseTime(endsAt.String)
		n.EndsAt = &t
	}
	return n, nil
}

func (s *Store) SaveNet(n Net) (Net, error) {
	n.Name = strings.TrimSpace(n.Name)
	if n.Name == "" {
		return Net{}, fmt.Errorf("a net needs a name")
	}
	if n.StartsAt.IsZero() {
		return Net{}, fmt.Errorf("a net needs a start date and time")
	}
	var repeaterID, endsAt any
	if n.RepeaterID != "" {
		repeaterID = n.RepeaterID
	}
	if n.EndsAt != nil {
		endsAt = formatTime(*n.EndsAt)
	}
	if n.ID == "" {
		n.ID = newID()
		n.CreatedAt = time.Now()
		_, err := s.db.Exec(`INSERT INTO nets (id, name, frequency, repeater_id, mode, starts_at, recur_kind,
				recur_weekdays_mask, recur_nth_week, recur_nth_weekday, ends_at, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			n.ID, n.Name, n.Frequency, repeaterID, n.Mode, formatTime(n.StartsAt), n.Recur,
			n.WeekdaysMask, n.NthWeek, int(n.NthWeekday), endsAt, n.CreatedBy, formatTime(n.CreatedAt))
		return n, err
	}
	_, err := s.db.Exec(`UPDATE nets SET name = ?, frequency = ?, repeater_id = ?, mode = ?, starts_at = ?,
			recur_kind = ?, recur_weekdays_mask = ?, recur_nth_week = ?, recur_nth_weekday = ?, ends_at = ?
		WHERE id = ?`,
		n.Name, n.Frequency, repeaterID, n.Mode, formatTime(n.StartsAt),
		n.Recur, n.WeekdaysMask, n.NthWeek, int(n.NthWeekday), endsAt, n.ID)
	return n, err
}

func (s *Store) DeleteNet(id string) error {
	res, err := s.db.Exec("DELETE FROM nets WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- recurrence math ----------
//
// No occurrence rows are ever materialized: the next occurrence is always
// computed on demand from the rule. maxRecurYears bounds every search loop
// so a malformed rule can never hang the scheduler.
const maxRecurYears = 5

// nextOccurrenceOnOrAfter returns the next time the net occurs at or after
// `after`, honoring an optional end date. ok is false once the series is
// exhausted (a one-off net whose time has passed, or past EndsAt).
func nextOccurrenceOnOrAfter(n Net, after time.Time) (time.Time, bool) {
	if n.EndsAt != nil && after.After(*n.EndsAt) {
		return time.Time{}, false
	}
	limit := after.AddDate(maxRecurYears, 0, 0)

	clampToTimeOfDay := func(day time.Time) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(),
			n.StartsAt.Hour(), n.StartsAt.Minute(), n.StartsAt.Second(), 0, n.StartsAt.Location())
	}

	switch n.Recur {
	case RecurOnce:
		if n.StartsAt.Before(after) {
			return time.Time{}, false
		}
		return n.StartsAt, true

	case RecurDaily:
		return firstOnOrAfter(n.StartsAt, after, 24*time.Hour, n.EndsAt)

	case RecurEveryOther:
		return firstOnOrAfter(n.StartsAt, after, 48*time.Hour, n.EndsAt)

	case RecurWeekly:
		mask := n.WeekdaysMask
		if mask == 0 {
			mask = weekdayBit(n.StartsAt.Weekday())
		}
		start := n.StartsAt
		if start.Before(after) {
			start = clampToTimeOfDay(after)
			if start.Before(after) {
				start = start.Add(24 * time.Hour)
			}
		}
		for d := start; d.Before(limit); d = d.AddDate(0, 0, 1) {
			if mask&weekdayBit(d.Weekday()) == 0 {
				continue
			}
			cand := clampToTimeOfDay(d)
			if cand.Before(after) || cand.Before(n.StartsAt) {
				continue
			}
			if n.EndsAt != nil && cand.After(*n.EndsAt) {
				return time.Time{}, false
			}
			return cand, true
		}
		return time.Time{}, false

	case RecurMonthlyDate:
		day := n.StartsAt.Day()
		y, m := after.Year(), after.Month()
		for i := 0; i < 12*maxRecurYears; i++ {
			cand := clampToTimeOfDay(monthlyDate(y, m, day))
			if !cand.Before(after) && !cand.Before(n.StartsAt) {
				if n.EndsAt != nil && cand.After(*n.EndsAt) {
					return time.Time{}, false
				}
				return cand, true
			}
			m++
			if m > 12 {
				m = 1
				y++
			}
		}
		return time.Time{}, false

	case RecurMonthlyNth:
		y, m := after.Year(), after.Month()
		for i := 0; i < 12*maxRecurYears; i++ {
			day, ok := nthWeekdayOfMonth(y, m, n.NthWeekday, n.NthWeek)
			if ok {
				cand := clampToTimeOfDay(day)
				if !cand.Before(after) && !cand.Before(n.StartsAt) {
					if n.EndsAt != nil && cand.After(*n.EndsAt) {
						return time.Time{}, false
					}
					return cand, true
				}
			}
			m++
			if m > 12 {
				m = 1
				y++
			}
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// firstOnOrAfter finds the first `start + k*step` (k >= 0) that is not before
// `after`. A pure O(1) computation — no loop, so no bound is needed here.
func firstOnOrAfter(start, after time.Time, step time.Duration, endsAt *time.Time) (time.Time, bool) {
	cand := start
	if after.After(start) {
		k := after.Sub(start) / step
		cand = start.Add(k * step)
		if cand.Before(after) {
			cand = cand.Add(step)
		}
	}
	if endsAt != nil && cand.After(*endsAt) {
		return time.Time{}, false
	}
	return cand, true
}

func monthlyDate(y int, m time.Month, day int) time.Time {
	// time.Date normalizes an out-of-range day (e.g. day 31 in April) by
	// rolling into the next month, so clamp to the last real day instead —
	// a net "on the 31st" runs on the 30th in a 30-day month.
	firstOfNext := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	lastDay := firstOfNext.AddDate(0, 0, -1).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

// nthWeekdayOfMonth finds the date of the nth (1-5, 5=last) given weekday in
// a month. ok is false if there is no 5th occurrence that month.
func nthWeekdayOfMonth(y int, m time.Month, wd time.Weekday, nth int) (time.Time, bool) {
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	offset := (int(wd) - int(first.Weekday()) + 7) % 7
	firstMatch := first.AddDate(0, 0, offset)
	if nth == 5 { // "last"
		last := firstMatch
		for {
			next := last.AddDate(0, 0, 7)
			if next.Month() != m {
				return last, true
			}
			last = next
		}
	}
	day := firstMatch.AddDate(0, 0, 7*(nth-1))
	if day.Month() != m {
		return time.Time{}, false
	}
	return day, true
}

// ---------- reminder scheduler ----------

const reminderCheckWindow = time.Minute

// checkReminders looks at every net's next occurrence and every opted-in
// member's lead time, and sends (and dedups) a reminder for anything due in
// this tick. It is pure enough to unit test with a fabricated `now`: nothing
// here reads the wall clock directly.
func (a *App) checkReminders(now time.Time) {
	nets := a.store.Nets()
	if len(nets) == 0 {
		return
	}
	members := a.store.All()
	var optedIn []*Member
	for _, m := range members {
		if m.NetRemindOptIn && m.NetRemindLeadMinutes > 0 && accessOf(m) >= AccessMember {
			optedIn = append(optedIn, m)
		}
	}
	if len(optedIn) == 0 {
		return
	}
	for _, n := range nets {
		occ, ok := nextOccurrenceOnOrAfter(n, now.Add(-time.Hour)) // catch anything just-missed too
		if !ok {
			continue
		}
		for _, m := range optedIn {
			due := occ.Add(-time.Duration(m.NetRemindLeadMinutes) * time.Minute)
			if now.Before(due) || now.Sub(due) > reminderCheckWindow {
				continue
			}
			a.sendNetReminder(n, m, occ)
		}
	}
}

func (a *App) sendNetReminder(n Net, m *Member, occ time.Time) {
	sent, err := a.store.markReminderSent(n.ID, occ, m.ID)
	if err != nil {
		log.Printf("reminder dedup check for %s: %v", m.Email, err)
		return
	}
	if !sent {
		return // already sent for this exact occurrence
	}
	repeater := ""
	if n.RepeaterID != "" {
		if r, err := a.store.RepeaterByID(n.RepeaterID); err == nil {
			repeater = " via " + r.Name
		}
	}
	mode := ""
	if n.Mode != "" {
		mode = " (" + n.Mode + ")"
	}
	subject := fmt.Sprintf("Reminder: %s starts in %d minutes", n.Name, m.NetRemindLeadMinutes)
	body := fmt.Sprintf(`Hello,

%s starts at %s on %s%s%s.

— NetRemind
`, n.Name, occ.Format("15:04 MST"), n.Frequency, repeater, mode)
	if err := a.mailer.Send(m.Email, subject, body); err != nil {
		log.Printf("net reminder to %s: %v", m.Email, err)
	}
}

// runReminderScheduler ticks once a minute for the life of the process. Not
// started in tests (testApp never calls it) — tests call checkReminders
// directly with a fabricated `now` instead, for determinism.
func (a *App) runReminderScheduler() {
	for range time.Tick(time.Minute) {
		a.checkReminders(time.Now())
	}
}

// markReminderSent records the send in reminder_log and reports whether this
// call was the one that actually inserted it (false = already sent).
func (s *Store) markReminderSent(netID string, occurrence time.Time, memberID string) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO reminder_log (net_id, occurrence_at, member_id, sent_at)
		VALUES (?, ?, ?, ?)`, netID, formatTime(occurrence), memberID, formatTime(time.Now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
