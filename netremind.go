package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// NetRemind lets a Net Admin schedule club nets and members subscribe to
// email reminders. A net has a recurrence pattern (daily, weekdays,
// weekends, weekly on N weekdays, every N weeks on one weekday, or monthly
// on the Nth weekday), a start time, and an optional description. The
// reminder mailer computes upcoming occurrences and notifies subscribers.

type Recur string

const (
	RecurDaily    Recur = "daily"
	RecurWeekdays Recur = "weekdays"
	RecurWeekends Recur = "weekends"
	RecurWeekly   Recur = "weekly"   // weekly on one or more weekdays
	RecurBiweekly Recur = "biweekly" // every N weeks on one weekday
	RecurMonthly  Recur = "monthly"  // nth <weekday> of each month
)

var allRecurs = []Recur{RecurDaily, RecurWeekdays, RecurWeekends, RecurWeekly, RecurBiweekly, RecurMonthly}

func (r Recur) Label() string {
	switch r {
	case RecurDaily:
		return "Every day"
	case RecurWeekdays:
		return "Weekdays (Mon–Fri)"
	case RecurWeekends:
		return "Weekends (Sat & Sun)"
	case RecurWeekly:
		return "Weekly"
	case RecurBiweekly:
		return "Every N weeks"
	case RecurMonthly:
		return "Monthly (nth weekday)"
	}
	return string(r)
}

// Weekday names stored as English short names ("Mon".."Sun") so the DB is
// human-readable; Go's time.Weekday round-trips through this map.
var weekdayNames = map[time.Weekday]string{
	time.Sunday: "Sun", time.Monday: "Mon", time.Tuesday: "Tue",
	time.Wednesday: "Wed", time.Thursday: "Thu", time.Friday: "Fri",
	time.Saturday: "Sat",
}

var weekdayByShort = map[string]time.Weekday{
	"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday,
	"Wed": time.Wednesday, "Thu": time.Thursday, "Fri": time.Friday,
	"Sat": time.Saturday,
}

func parseWeekdays(csv string) []time.Weekday {
	out := []time.Weekday{}
	for _, p := range strings.Split(csv, ",") {
		p = strings.TrimSpace(p)
		if d, ok := weekdayByShort[p]; ok {
			out = append(out, d)
		}
	}
	return out
}

func weekdaysToString(ds []time.Weekday) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		parts = append(parts, weekdayNames[d])
	}
	return strings.Join(parts, ",")
}

type Net struct {
	ID          int64
	Name        string
	Description string
	Recur       Recur
	Weekdays    []time.Weekday // weekly: chosen days; biweekly/monthly: the one day
	EveryNWeeks int            // biweekly: interval (2 = every two weeks)
	NthWeek     int            // monthly: 1..5 (5 = last of the month)
	LocalTime   string         // "HH:MM" in the server's local time
	StartTime   time.Time      // creation timestamp; occurrences derive from Recur
	Active      bool
	CreatedBy   string // member ID of the net admin who created it
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Describe renders a human-readable schedule line, e.g. "Weekly on Mon, Wed at 19:30".
func (n *Net) Describe() string {
	var pattern string
	switch n.Recur {
	case RecurDaily:
		pattern = "every day"
	case RecurWeekdays:
		pattern = "on weekdays"
	case RecurWeekends:
		pattern = "on weekends"
	case RecurWeekly:
		pattern = "weekly on " + joinDayNames(n.Weekdays)
	case RecurBiweekly:
		if n.EveryNWeeks == 2 {
			pattern = "every two weeks on " + joinDayNames(n.Weekdays)
		} else {
			pattern = fmt.Sprintf("every %d weeks on %s", n.EveryNWeeks, joinDayNames(n.Weekdays))
		}
	case RecurMonthly:
		ord := map[int]string{1: "first", 2: "second", 3: "third", 4: "fourth", 5: "last"}[n.NthWeek]
		pattern = fmt.Sprintf("the %s %s of each month", ord, joinDayNames(n.Weekdays))
	default:
		pattern = string(n.Recur)
	}
	return fmt.Sprintf("%s, %s local time", pattern, n.LocalTime)
}

func joinDayNames(ds []time.Weekday) string {
	names := make([]string, 0, len(ds))
	for _, d := range ds {
		names = append(names, weekdayNames[d])
	}
	if len(names) == 0 {
		return "—"
	}
	return strings.Join(names, ", ")
}

// ---------- occurrence computation ----------

// nextOccurrence returns the next occurrence of the net strictly after t,
// in the server's local time. It scans forward day by day (bounded), which
// is plenty for these simple patterns.
func (n *Net) nextOccurrence(after time.Time) (time.Time, bool) {
	// Find days matching the pattern, then apply the time-of-day.
	hh, mm := 19, 30
	if t, err := time.Parse("15:04", n.LocalTime); err == nil {
		hh, mm = t.Hour(), t.Minute()
	}
	// Start checking from the day after `after` unless today's time is still ahead.
	day := time.Date(after.Year(), after.Month(), after.Day(), hh, mm, 0, 0, after.Location())
	if !day.After(after) {
		day = day.AddDate(0, 0, 1)
	}
	// Anchor for biweekly: the Monday of the week the net was created, so
	// the every-N-weeks cycle stays stable regardless of when we ask.
	anchor := mondayOf(n.CreatedAt)

	for i := 0; i < 400; i++ { // bound: ~1 year of daily steps
		if n.matchesDay(day, anchor) {
			return day, true
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, false
}

func mondayOf(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	offset := (int(d.Weekday()) + 6) % 7 // Mon=0
	return d.AddDate(0, 0, -offset)
}

func (n *Net) matchesDay(day time.Time, anchor time.Time) bool {
	switch n.Recur {
	case RecurDaily:
		return true
	case RecurWeekdays:
		return day.Weekday() >= time.Monday && day.Weekday() <= time.Friday
	case RecurWeekends:
		return day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
	case RecurWeekly:
		for _, d := range n.Weekdays {
			if day.Weekday() == d {
				return true
			}
		}
		return false
	case RecurBiweekly:
		n_ := n.EveryNWeeks
		if n_ < 1 {
			n_ = 1
		}
		for _, d := range n.Weekdays {
			if day.Weekday() != d {
				continue
			}
			weeks := int(mondayOf(day).Sub(anchor).Hours() / (7 * 24))
			return weeks%n_ == 0
		}
		return false
	case RecurMonthly:
		if len(n.Weekdays) == 0 || day.Weekday() != n.Weekdays[0] {
			return false
		}
		return nthWeekdayOfMonth(day, n.Weekdays[0], n.NthWeek)
	}
	return false
}

// nthWeekdayOfMonth reports whether day is the nth <wd> of its month
// (n=5 means the last one).
func nthWeekdayOfMonth(day time.Time, wd time.Weekday, n int) bool {
	if day.Weekday() != wd {
		return false
	}
	if n >= 1 && n <= 4 {
		return (day.Day()-1)/7+1 == n
	}
	// last: is the next same-weekday within 7 days in a different month?
	next := day.AddDate(0, 0, 7)
	return next.Month() != day.Month()
}

// upcoming lists the next count occurrences after t.
func (n *Net) upcoming(t time.Time, count int) []time.Time {
	out := []time.Time{}
	cur := t
	for i := 0; i < count; i++ {
		next, ok := n.nextOccurrence(cur)
		if !ok {
			break
		}
		out = append(out, next)
		cur = next
	}
	return out
}

// ---------- persistence ----------

const netSchema = `
CREATE TABLE IF NOT EXISTS nets (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	recur        TEXT NOT NULL,
	weekdays     TEXT NOT NULL DEFAULT '',
	every_n_weeks INTEGER NOT NULL DEFAULT 2,
	nth_week     INTEGER NOT NULL DEFAULT 1,
	local_time   TEXT NOT NULL DEFAULT '19:30',
	active       INTEGER NOT NULL DEFAULT 1,
	created_by   TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS net_subscriptions (
	net_id       INTEGER NOT NULL REFERENCES nets(id) ON DELETE CASCADE,
	member_id    TEXT NOT NULL REFERENCES members(id) ON DELETE CASCADE,
	created_at   TEXT NOT NULL,
	PRIMARY KEY (net_id, member_id)
);
`

var netMigrations = []string{
	// none yet beyond initial creation
}

func openNetTables(db *sql.DB) error {
	if _, err := db.Exec(netSchema); err != nil {
		return err
	}
	for _, m := range netMigrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

func scanNet(row scanner) (*Net, error) {
	var n Net
	var weekdays string
	var active int
	var createdAt, updatedAt string
	err := row.Scan(&n.ID, &n.Name, &n.Description, &n.Recur, &weekdays,
		&n.EveryNWeeks, &n.NthWeek, &n.LocalTime, &active, &n.CreatedBy,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	n.Weekdays = parseWeekdays(weekdays)
	n.Active = active != 0
	n.CreatedAt = parseTime(createdAt)
	n.UpdatedAt = parseTime(updatedAt)
	return &n, nil
}

const netCols = "id, name, description, recur, weekdays, every_n_weeks, nth_week, local_time, active, created_by, created_at, updated_at"

func (n *Net) values() []any {
	return []any{
		n.Name, n.Description, string(n.Recur), weekdaysToString(n.Weekdays),
		n.EveryNWeeks, n.NthWeek, n.LocalTime, boolToInt(n.Active), n.CreatedBy,
		formatTime(n.CreatedAt), formatTime(n.UpdatedAt),
	}
}

func (s *Store) CreateNet(n *Net) (*Net, error) {
	now := time.Now()
	n.CreatedAt = now
	n.UpdatedAt = now
	if n.LocalTime == "" {
		n.LocalTime = "19:30"
	}
	if n.EveryNWeeks < 1 {
		n.EveryNWeeks = 2
	}
	if n.NthWeek < 1 || n.NthWeek > 5 {
		n.NthWeek = 1
	}
	args := n.values()
	res, err := s.db.Exec(`INSERT INTO nets (name, description, recur, weekdays, every_n_weeks, nth_week, local_time, active, created_by, created_at, updated_at)
		VALUES (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.NetByID(id)
}

func (s *Store) UpdateNet(id int64, fn func(*Net) error) (*Net, error) {
	n, err := s.NetByID(id)
	if err != nil {
		return nil, err
	}
	if err := fn(n); err != nil {
		return nil, err
	}
	n.UpdatedAt = time.Now()
	if n.LocalTime == "" {
		n.LocalTime = "19:30"
	}
	if n.EveryNWeeks < 1 {
		n.EveryNWeeks = 2
	}
	if n.NthWeek < 1 || n.NthWeek > 5 {
		n.NthWeek = 1
	}
	args := append(n.values(), id)
	_, err = s.db.Exec(`UPDATE nets SET name=?, description=?, recur=?, weekdays=?, every_n_weeks=?, nth_week=?, local_time=?, active=?, created_by=?, created_at=?, updated_at=? WHERE id=?`, args...)
	if err != nil {
		return nil, err
	}
	return s.NetByID(id)
}

func (s *Store) NetByID(id int64) (*Net, error) {
	row := s.db.QueryRow("SELECT "+netCols+" FROM nets WHERE id = ?", id)
	n, err := scanNet(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return n, err
}

// Nets lists all nets; activeOnly trims to active ones. Ordered by the next
// occurrence is not possible in SQL, so the caller sorts.
func (s *Store) Nets(activeOnly bool) []*Net {
	q := "SELECT " + netCols + " FROM nets"
	if activeOnly {
		q += " WHERE active = 1"
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []*Net{}
	for rows.Next() {
		n, err := scanNet(rows)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (s *Store) DeleteNet(id int64) error {
	_, err := s.db.Exec("DELETE FROM nets WHERE id = ?", id)
	return err
}

// ---------- subscriptions ----------

func (s *Store) Subscribe(memberID string, netID int64) error {
	_, err := s.db.Exec(`INSERT INTO net_subscriptions (net_id, member_id, created_at) VALUES (?, ?, ?)
		ON CONFLICT (net_id, member_id) DO NOTHING`, netID, memberID, formatTime(time.Now()))
	return err
}

func (s *Store) Unsubscribe(memberID string, netID int64) error {
	_, err := s.db.Exec("DELETE FROM net_subscriptions WHERE net_id = ? AND member_id = ?", netID, memberID)
	return err
}

// SubscribedNetIDs returns the net ids a member is subscribed to.
func (s *Store) SubscribedNetIDs(memberID string) map[int64]bool {
	rows, err := s.db.Query("SELECT net_id FROM net_subscriptions WHERE member_id = ?", memberID)
	out := map[int64]bool{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// Subscribers returns the member IDs subscribed to a net.
func (s *Store) Subscribers(netID int64) []string {
	rows, err := s.db.Query("SELECT member_id FROM net_subscriptions WHERE net_id = ?", netID)
	var out []string
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// ---------- reminder sending ----------

// SendNetRemindersForDay emails every opted-in subscriber of every net that
// occurs on the given local day. Returns the number of reminders sent.
// Designed to run once per day shortly after midnight; sending the day's
// reminders "for today" keeps the wording simple ("tonight at 19:30").
func (a *App) SendNetRemindersForDay(day time.Time) int {
	sent := 0
	for _, n := range a.store.Nets(true) {
		if !n.matchesDay(day, mondayOf(n.CreatedAt)) {
			continue
		}
		for _, mid := range a.store.Subscribers(n.ID) {
			m, err := a.store.ByID(mid)
			if err != nil || !m.NetRemind || accessOf(m) == AccessNone {
				continue
			}
			a.sendNetReminder(m, n, day)
			sent++
		}
	}
	return sent
}

func (a *App) sendNetReminder(m *Member, n *Net, day time.Time) {
	set := a.store.Settings()
	subject := fmt.Sprintf("Net reminder: %s — %s %s",
		n.Name, day.Format("Mon 2 Jan"), n.LocalTime)
	body := fmt.Sprintf(`Hello %s,

A reminder that %s meets %s.

%s

73,
%s
`, m.Display(), n.Name, n.Describe(),
		orDash(n.Description), set.ClubName)
	if err := a.mailer.Send(m.Email, subject, body); err != nil {
		a.audit.Write("system", "netremind.failed", m.ID, fmt.Sprintf("net %d", n.ID), "")
	}
}

// NetRemindSubscribers lists members opted in to net reminders (for the
// nets page's subscriber count display).
func (s *Store) NetRemindOptIns() int {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM members WHERE net_remind = 1 AND status = ?", string(StatusActive)).Scan(&n)
	return n
}

// ---------- in-process daily scheduler ----------

var stopReminder = make(chan struct{})

// runReminderLoop fires SendNetRemindersForDay once per local day at
// hh:mm. It tolerates a missed window (container asleep at the tick):
// on startup it first checks whether today's reminders were already sent
// today by a previous process and, if not and the scheduled time has
// passed, sends them immediately so a restart never loses a day.
func runReminderLoop(a *App, at string, stop <-chan struct{}) {
	hh, mm := 8, 0
	if t, err := time.Parse("15:04", at); err == nil {
		hh, mm = t.Hour(), t.Minute()
	} else {
		log.Printf("net reminders: invalid -remind-at %q, defaulting to 08:00", at)
	}

	// Catch-up on startup: if the scheduled time already passed today,
	// today's reminders have not been sent by us, and the log shows no
	// earlier send today, send them now.
	if time.Now().After(todayAt(hh, mm)) {
		n := a.SendNetRemindersForDay(time.Now())
		if n > 0 {
			log.Printf("net reminders (catch-up) sent: %d", n)
		}
	}

	for {
		next := todayAt(hh, mm)
		if !next.After(time.Now()) {
			next = next.AddDate(0, 0, 1)
		}
		log.Printf("net reminders: next run %s", next.Format("2006-01-02 15:04"))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		n := a.SendNetRemindersForDay(time.Now())
		if n > 0 {
			log.Printf("net reminders sent: %d", n)
		}
	}
}

func todayAt(hh, mm int) time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
}
