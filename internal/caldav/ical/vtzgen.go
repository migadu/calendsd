package ical

// VTIMEZONE generation for objects that use an IANA TZID without defining it
// (docs/dst-recurring-events.md §5). RFC 5545 §3.6.5 requires a VTIMEZONE for
// every TZID an object uses; calweb and other clients send bare IANA names.
//
// The definition must agree with how this server expands the object, and
// expansion resolves TZIDs through time.LoadLocation (go-ical). So the
// generator works from that same *time.Location:
//
//   - Transitions come from Location itself via Time.ZoneBounds — exact, and
//     including historical rule changes; no year is sampled or extrapolated.
//   - A zone that keeps changing forever (a DST rule) needs open-ended
//     observances. Their rule is the tzdb's own: the POSIX TZ footer of the
//     zone's TZif file. It is used only after reproducing the Location's
//     transitions for 100 years; otherwise generation fails rather than attach
//     an approximation.
//   - Definitions are cached by TZID, a fingerprint of the Location's
//     transitions (so a tzdb change on disk, which LoadLocation picks up on its
//     next read, never reuses a stale definition) and coverage.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	goical "github.com/emersion/go-ical"
)

// Coverage is the span of dates a VTIMEZONE must describe. Unbounded means the
// object recurs forever (or for a COUNT we don't evaluate), so the definition
// needs open-ended future observances.
type Coverage struct {
	Start     time.Time
	End       time.Time
	Unbounded bool
}

func (c Coverage) contains(o Coverage) bool {
	if o.Start.Before(c.Start) {
		return false
	}
	if c.Unbounded {
		return true
	}
	return !o.Unbounded && !o.End.After(c.End)
}

// transition is one offset change: at the instant `at`, the zone switches from
// offset `from` to the zone (name, offset, dst) in effect from then on.
type transition struct {
	at   time.Time
	from int
	to   int
	name string
	dst  bool
}

// horizon bounds enumeration; a zone still changing after it has a perpetual
// rule.
var horizon = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)

// zoneTransitions lists the Location's transitions after `from` up to `to`
// (inclusive), exactly. It follows Time.ZoneBounds, skipping bounds that
// change nothing (past a zone's explicit data Go splits its rule-generated
// zones at year ends). Go's bounds are unreliable around 31 December of
// rule-extended leap years (they stop advancing), while its offset lookups
// stay correct; where a bound doesn't advance, the walk steps a day by lookup
// and bisects to the second if anything changed within it.
func zoneTransitions(loc *time.Location, from, to time.Time) []transition {
	var out []transition
	t := from.In(loc)
	record := func(at time.Time) {
		prevName, prevOff := at.Add(-time.Second).Zone()
		name, off := at.Zone()
		if off != prevOff || name != prevName || at.IsDST() != at.Add(-time.Second).IsDST() {
			out = append(out, transition{at: at.UTC(), from: prevOff, to: off, name: name, dst: at.IsDST()})
		}
	}
	for i := 0; i < 100000; i++ {
		_, end := t.ZoneBounds()
		if !end.IsZero() && end.After(t) {
			if end.After(to) {
				break
			}
			record(end)
			t = end
			continue
		}
		// No usable bound: step a day by offset lookup.
		next := t.Add(24 * time.Hour)
		if !sameZone(t, next) {
			lo, hi := t, next
			for hi.Sub(lo) > time.Second {
				mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Second)
				if !mid.After(lo) {
					mid = lo.Add(time.Second)
				}
				if sameZone(lo, mid) {
					lo = mid
				} else {
					hi = mid
				}
			}
			next = hi
		}
		if next.After(to) {
			break
		}
		if end.IsZero() && sameZone(t, next) {
			// A genuinely unbounded zone (no further changes at all).
			if _, e := next.ZoneBounds(); e.IsZero() {
				break
			}
		}
		record(next)
		t = next
	}
	return out
}

func sameZone(a, b time.Time) bool {
	an, ao := a.Zone()
	bn, bo := b.Zone()
	return an == bn && ao == bo && a.IsDST() == b.IsDST()
}

// fingerprint identifies what the Location says, not where it came from: a
// hash of its transitions over the range any object plausibly uses.
func fingerprint(loc *time.Location) string {
	h := sha256.New()
	start := time.Date(1850, 1, 1, 0, 0, 0, 0, time.UTC)
	name, off := start.In(loc).Zone()
	fmt.Fprintf(h, "%s|%d|", name, off)
	for _, tr := range zoneTransitions(loc, start, horizon) {
		fmt.Fprintf(h, "%d|%d|%s|%t;", tr.at.Unix(), tr.to, tr.name, tr.dst)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

type vtzCacheEntry struct {
	cov   Coverage
	comp  *goical.Component
	print string
}

// vtzCache holds at most one definition per TZID: open-ended, from the
// earliest year any object asked for. Requests are normalized to that shape,
// so objects on different dates share one entry instead of each adding its
// own, and memory stays bounded by the number of zones in use.
var vtzCache = struct {
	sync.Mutex
	m map[string]vtzCacheEntry
}{m: map[string]vtzCacheEntry{}}

// GenerateVTimezone builds a VTIMEZONE for an IANA zone covering at least
// cov, from the same Location expansion uses. It fails, rather than
// approximate, when the zone's rules can't be represented faithfully.
func GenerateVTimezone(tzid string, cov Coverage) (*goical.Component, error) {
	loc, err := time.LoadLocation(tzid)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", tzid)
	}
	want := Coverage{Start: time.Date(cov.Start.Year(), 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1), Unbounded: true}
	vtzCache.Lock()
	e, ok := vtzCache.m[tzid]
	vtzCache.Unlock()
	// The fingerprint walks the zone's transitions, so it is only taken to
	// compare with a cached entry or to store a new one.
	var print string
	if ok {
		print = fingerprint(loc)
	}
	if ok && e.print == print {
		if e.cov.contains(cov) {
			return e.comp, nil
		}
		if e.cov.Start.Before(want.Start) {
			want.Start = e.cov.Start
		}
	}
	comp, err := generateChecked(tzid, loc, want)
	if err != nil {
		if cov.Unbounded {
			return nil, err
		}
		// A zone whose future rule can't be written still gets an exact
		// definition for a bounded object — not cached, as it is specific to
		// this object's dates.
		return generateChecked(tzid, loc, cov)
	}
	if print == "" {
		print = fingerprint(loc)
	}
	vtzCache.Lock()
	vtzCache.m[tzid] = vtzCacheEntry{cov: want, comp: comp, print: print}
	vtzCache.Unlock()
	return comp, nil
}

// generateChecked builds a definition and reads it back with the VTIMEZONE
// parser the server uses for zones it can't load by name, requiring it to
// give the Location's offsets across the whole coverage.
func generateChecked(tzid string, loc *time.Location, cov Coverage) (*goical.Component, error) {
	comp, err := buildVTimezone(tzid, loc, cov)
	if err != nil {
		return nil, err
	}
	if err := validateVTimezone(comp, loc, cov); err != nil {
		return nil, fmt.Errorf("time zone %s: %w", tzid, err)
	}
	return comp, nil
}

func buildVTimezone(tzid string, loc *time.Location, cov Coverage) (*goical.Component, error) {
	start := cov.Start.In(loc)
	// The observance in effect at the start: its onset and the offset before.
	onset, _ := start.ZoneBounds()
	name, off := start.Zone()
	first := transition{name: name, to: off, from: off, dst: start.IsDST()}
	if !onset.IsZero() {
		_, first.from = onset.Add(-time.Second).Zone()
		first.at = onset.UTC()
	}
	trs := []transition{first}

	perpetual := false
	if _, end := horizon.In(loc).ZoneBounds(); !end.IsZero() {
		perpetual = true
	}
	switch {
	case !cov.Unbounded:
		trs = append(trs, zoneTransitions(loc, start, cov.End)...)
	case !perpetual:
		// The zone stops changing: list every remaining transition; the last
		// observance is its permanent final offset.
		trs = append(trs, zoneTransitions(loc, start, horizon)...)
	default:
		rule, lastExplicit, err := futureRule(tzid, loc)
		if err != nil {
			return nil, err
		}
		// Explicit transitions up to where the tzdb's own list ends, then the
		// rule — its observances onset no later than a year before the start,
		// so one of them is in effect there. An observance at the start that
		// the rule itself produces isn't listed twice.
		if first.at.After(lastExplicit) {
			trs = trs[:0]
		}
		ruleFrom := lastExplicit
		if yearBefore := cov.Start.AddDate(-1, 0, 0); yearBefore.After(ruleFrom) {
			ruleFrom = yearBefore
		}
		trs = append(trs, zoneTransitions(loc, start, lastExplicit)...)
		return renderVTimezone(tzid, trs, &rule, ruleFrom)
	}
	return renderVTimezone(tzid, trs, nil, time.Time{})
}

// validateHorizon is how far an unbounded definition is checked — the reader
// expands RRULE observances up to 2100.
var validateHorizon = time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC)

// validateVTimezone checks a generated definition against the Location at the
// start of the coverage and on both sides of every transition in it.
func validateVTimezone(comp *goical.Component, loc *time.Location, cov Coverage) error {
	z := parseVTimezone(comp)
	if z == nil {
		return errors.New("generated definition has no observances")
	}
	end := cov.End
	if cov.Unbounded {
		end = validateHorizon
	}
	check := func(t time.Time) error {
		_, want := t.In(loc).Zone()
		if got := z.offsetAtInstant(t); got != want {
			return fmt.Errorf("generated definition gives offset %s at %s, the server uses %s",
				formatUTCOffset(got), t.Format(time.RFC3339), formatUTCOffset(want))
		}
		return nil
	}
	if err := check(cov.Start); err != nil {
		return err
	}
	for _, tr := range zoneTransitions(loc, cov.Start, end) {
		if err := check(tr.at.Add(-time.Second)); err != nil {
			return err
		}
		if err := check(tr.at); err != nil {
			return err
		}
	}
	return nil
}

// renderVTimezone groups explicit transitions by observance (DTSTART + RDATEs)
// and, given a future rule, adds its yearly RRULE observances with onsets
// after ruleFrom.
func renderVTimezone(tzid string, trs []transition, rule *posixRule, ruleFrom time.Time) (*goical.Component, error) {
	vtz := goical.NewComponent(goical.CompTimezone)
	setRaw(vtz, goical.PropTimezoneID, tzid)

	type key struct {
		dst      bool
		from, to int
		name     string
	}
	groups := map[key][]time.Time{}
	var order []key
	for _, tr := range trs {
		k := key{tr.dst, tr.from, tr.to, tr.name}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], localOnset(tr))
	}
	for _, k := range order {
		sub := observance(k.dst, k.from, k.to, k.name)
		times := groups[k]
		setRaw(sub, goical.PropDateTimeStart, formatLocal(times[0]))
		if len(times) > 1 {
			vals := make([]string, 0, len(times)-1)
			for _, t := range times[1:] {
				vals = append(vals, formatLocal(t))
			}
			setRaw(sub, goical.PropRecurrenceDates, strings.Join(vals, ","))
		}
		vtz.Children = append(vtz.Children, sub)
	}
	if rule == nil {
		return vtz, nil
	}
	// One observance per yearly RRULE part: most rules have one per kind; a
	// rule whose dates straddle two months has one per month (see parts).
	for _, kind := range []struct {
		dst bool
		m   mRule
	}{{true, rule.start}, {false, rule.end}} {
		parts, err := kind.m.parts()
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			// DTSTART: the first of this part's dates after ruleFrom (a date
			// in the other month's part can be years away; the calendar
			// repeats within 400 years).
			var first *transition
			for y := ruleFrom.Year(); y <= ruleFrom.Year()+400 && first == nil; y++ {
				for _, tr := range rule.transitionsIn(y) {
					if tr.dst == kind.dst && tr.at.After(ruleFrom) && int(localOnset(tr).Month()) == part.month {
						tr := tr
						first = &tr
						break
					}
				}
			}
			if first == nil {
				continue
			}
			sub := observance(first.dst, first.from, first.to, first.name)
			setRaw(sub, goical.PropDateTimeStart, formatLocal(localOnset(*first)))
			setRaw(sub, goical.PropRecurrenceRule, part.rrule)
			vtz.Children = append(vtz.Children, sub)
		}
	}
	return vtz, nil
}

func observance(dst bool, from, to int, name string) *goical.Component {
	kind := goical.CompTimezoneStandard
	if dst {
		kind = goical.CompTimezoneDaylight
	}
	sub := goical.NewComponent(kind)
	setRaw(sub, goical.PropTimezoneOffsetFrom, formatUTCOffset(from))
	setRaw(sub, goical.PropTimezoneOffsetTo, formatUTCOffset(to))
	if name != "" {
		setRaw(sub, goical.PropTimezoneName, name)
	}
	return sub
}

// setRaw sets a property's value verbatim, with no VALUE parameter (go-ical's
// SetText marks non-TEXT properties such as TZOFFSETFROM as VALUE=TEXT).
func setRaw(c *goical.Component, name, value string) {
	c.Props.Set(&goical.Prop{Name: name, Params: goical.Params{}, Value: value})
}

// localOnset is the transition's onset as wall time in the offset before it
// (RFC 5545: an observance's DTSTART is local time in TZOFFSETFROM). The zero
// transition (no onset known) is the conventional 1601-01-01.
func localOnset(tr transition) time.Time {
	if tr.at.IsZero() {
		return time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return tr.at.Add(time.Duration(tr.from) * time.Second)
}

func formatLocal(t time.Time) string { return t.UTC().Format("20060102T150405") }

func formatUTCOffset(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	s := fmt.Sprintf("%s%02d%02d", sign, sec/3600, sec%3600/60)
	if sec%60 != 0 {
		s += fmt.Sprintf("%02d", sec%60)
	}
	return s
}

// ---- the future rule -------------------------------------------------------

// posixRule is a POSIX TZ string's DST rule with M-form dates (Mm.w.d).
type posixRule struct {
	stdName, dstName string
	stdOff, dstOff   int // seconds east of UTC
	start, end       mRule
}

type mRule struct {
	month, week, day int // week 5 = last
	secs             int // local time of the change, in the offset before it
}

// dateIn is the rule's date in year y (UTC-based calendar arithmetic).
func (m mRule) dateIn(y int) time.Time {
	first := time.Date(y, time.Month(m.month), 1, 0, 0, 0, 0, time.UTC)
	d := 1 + (m.day-int(first.Weekday())+7)%7 + (m.week-1)*7
	if m.week == 5 {
		last := time.Date(y, time.Month(m.month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
		for d > last {
			d -= 7
		}
	}
	return time.Date(y, time.Month(m.month), d, 0, 0, 0, 0, time.UTC)
}

// transitionsIn returns the rule's two transitions in year y, in order.
func (r posixRule) transitionsIn(y int) []transition {
	on := r.start.dateIn(y).Add(time.Duration(r.start.secs-r.stdOff) * time.Second)
	off := r.end.dateIn(y).Add(time.Duration(r.end.secs-r.dstOff) * time.Second)
	a := transition{at: on, from: r.stdOff, to: r.dstOff, name: r.dstName, dst: true}
	b := transition{at: off, from: r.dstOff, to: r.stdOff, name: r.stdName, dst: false}
	if b.at.Before(a.at) {
		a, b = b, a
	}
	return []transition{a, b}
}

// rulePart is one yearly RRULE observance of a tzdb rule: the rule's dates
// that fall in `month`.
type rulePart struct {
	month int
	rrule string
}

// parts renders a tzdb rule Mm.w.d (the w-th — 5 = last — weekday d of
// month m) as yearly RRULEs. The tzdb allows the change time outside
// 00:00–24:00 (Jerusalem "M3.4.4/26": 02:00 on the Friday after the fourth
// Thursday; Cairo "M10.5.4/24": the day after the last Thursday), which moves
// the date to another weekday within a shifted range of month-days; that is
// written exactly as BYDAY=<weekday>;BYMONTHDAY=<range>. "Last" ranges use
// negative month-days, so February's length doesn't matter. A range that
// straddles two months becomes two observances, one per month: a single
// RRULE with BYMONTH=10,11 would apply the combined day list to both months
// and match extra dates. Each part matches at most once a year, and the parts
// together exactly once. Only ranges whose meaning depends on February's
// length (a positive range running past the 28th) are refused.
func (m mRule) parts() ([]rulePart, error) {
	days := []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}
	shift := m.secs / 86400
	if m.secs < 0 && m.secs%86400 != 0 {
		shift--
	}
	if shift == 0 {
		pos := strconv.Itoa(m.week)
		if m.week == 5 {
			pos = "-1"
		}
		return []rulePart{{m.month, fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYDAY=%s%s", m.month, pos, days[m.day])}}, nil
	}
	wd := days[((m.day+shift)%7+7)%7]
	part := func(month, lo, hi int) rulePart {
		var md []string
		for d := lo; d <= hi; d++ {
			md = append(md, strconv.Itoa(d))
		}
		return rulePart{month, fmt.Sprintf("FREQ=YEARLY;BYMONTH=%d;BYDAY=%s;BYMONTHDAY=%s", month, wd, strings.Join(md, ","))}
	}
	next, prev := m.month%12+1, (m.month+10)%12+1
	unsupported := errors.New("its daylight-saving rule changes on a date whose position depends on February's length, which a VTIMEZONE rule cannot express; include a VTIMEZONE in the object")
	if m.week == 5 {
		lo, hi := -7+shift, -1+shift // counted from the month's end
		switch {
		case hi <= -1 && lo >= -28:
			return []rulePart{part(m.month, lo, hi)}, nil
		case hi >= 0 && lo <= -1 && hi <= 6:
			return []rulePart{part(m.month, lo, -1), part(next, 1, hi+1)}, nil
		}
		return nil, unsupported
	}
	monthLen := time.Date(2001, time.Month(m.month)+1, 0, 0, 0, 0, 0, time.UTC).Day() // a non-leap year
	fixed := m.month != 2
	lo, hi := 7*(m.week-1)+1+shift, 7*m.week+shift
	switch {
	case lo >= 1 && (hi <= 28 || fixed && hi <= monthLen):
		return []rulePart{part(m.month, lo, hi)}, nil
	case lo < 1 && lo >= -6 && hi >= 1:
		// The previous month's last days, counted from its end.
		return []rulePart{part(prev, lo-1, -1), part(m.month, 1, hi)}, nil
	case fixed && lo >= 1 && lo <= monthLen && hi > monthLen:
		return []rulePart{part(m.month, lo, monthLen), part(next, 1, hi-monthLen)}, nil
	}
	return nil, unsupported
}

// futureRule finds the zone's POSIX TZ rule in a TZif file and checks that it
// reproduces the Location's transitions for 100 years after the file's last
// explicit transition. It returns the rule and that last explicit transition.
func futureRule(tzid string, loc *time.Location) (posixRule, time.Time, error) {
	var lastErr error
	for _, data := range tzifCandidates(tzid) {
		footer, lastExplicit, err := parseTZif(data)
		if err != nil {
			lastErr = err
			continue
		}
		rule, err := parsePosixTZ(footer)
		if err != nil {
			lastErr = fmt.Errorf("time zone %s: %w", tzid, err)
			continue
		}
		if err := verifyRule(rule, loc, lastExplicit); err != nil {
			lastErr = fmt.Errorf("time zone %s: %w", tzid, err)
			continue
		}
		return rule, lastExplicit, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("time zone %s: no tzdb file with a future rule was found", tzid)
	}
	return posixRule{}, time.Time{}, lastErr
}

// verifyRule compares the rule with the Location for 100 years.
func verifyRule(r posixRule, loc *time.Location, lastExplicit time.Time) error {
	until := lastExplicit.AddDate(100, 0, 0)
	want := zoneTransitions(loc, lastExplicit, until)
	var got []transition
	for y := lastExplicit.Year(); y <= until.Year(); y++ {
		for _, tr := range r.transitionsIn(y) {
			if tr.at.After(lastExplicit) && !tr.at.After(until) {
				got = append(got, tr)
			}
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("its tzdb rule does not reproduce the server's transitions (%d vs %d)", len(got), len(want))
	}
	for i := range got {
		if !got[i].at.Equal(want[i].at) || got[i].to != want[i].to || got[i].from != want[i].from {
			return fmt.Errorf("its tzdb rule disagrees with the server at %s", want[i].at.Format(time.RFC3339))
		}
	}
	return nil
}

// tzifCandidates returns the zone's TZif bytes from each place Go may load it
// from, in LoadLocation's order where known ($ZONEINFO, the platform
// directories, GOROOT's zoneinfo.zip). Which one LoadLocation used doesn't
// matter: a candidate is used only if it reproduces the Location.
func tzifCandidates(tzid string) [][]byte {
	if strings.Contains(tzid, "..") || strings.HasPrefix(tzid, "/") {
		return nil
	}
	var sources []string
	if env := os.Getenv("ZONEINFO"); env != "" {
		sources = append(sources, env)
	}
	sources = append(sources, "/usr/share/zoneinfo", "/usr/share/lib/zoneinfo",
		"/usr/lib/locale/TZ", "/etc/zoneinfo")
	if root := runtime.GOROOT(); root != "" {
		sources = append(sources, filepath.Join(root, "lib", "time", "zoneinfo.zip"))
	}
	var out [][]byte
	for _, src := range sources {
		if strings.HasSuffix(src, ".zip") {
			if b, err := readZipEntry(src, tzid); err == nil {
				out = append(out, b)
			}
			continue
		}
		if b, err := os.ReadFile(filepath.Join(src, tzid)); err == nil {
			out = append(out, b)
		}
	}
	return out
}

func readZipEntry(path, name string) ([]byte, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 1<<20))
		}
	}
	return nil, os.ErrNotExist
}

// parseTZif returns a TZif file's POSIX TZ footer and last explicit
// transition, from its 64-bit (v2+) data block.
func parseTZif(b []byte) (string, time.Time, error) {
	if len(b) < 44 || string(b[:4]) != "TZif" || b[4] < '2' {
		return "", time.Time{}, errors.New("not a version 2+ TZif file")
	}
	skip := func(hdr []byte, timeSize int) int {
		c := func(i int) int { return int(binary.BigEndian.Uint32(hdr[20+4*i:])) }
		isutcnt, isstdcnt, leapcnt, timecnt, typecnt, charcnt := c(0), c(1), c(2), c(3), c(4), c(5)
		return 44 + timecnt*timeSize + timecnt + typecnt*6 + charcnt + leapcnt*(timeSize+4) + isstdcnt + isutcnt
	}
	v1 := skip(b, 4)
	if len(b) < v1+44 {
		return "", time.Time{}, errors.New("truncated TZif file")
	}
	b2 := b[v1:]
	timecnt := int(binary.BigEndian.Uint32(b2[32:]))
	end := skip(b2, 8)
	if len(b2) < end+2 {
		return "", time.Time{}, errors.New("truncated TZif file")
	}
	var last time.Time
	if timecnt > 0 {
		last = time.Unix(int64(binary.BigEndian.Uint64(b2[44+8*(timecnt-1):])), 0).UTC()
	}
	rest := b2[end:]
	if rest[0] != '\n' {
		return "", time.Time{}, errors.New("TZif file has no footer")
	}
	nl := bytes.IndexByte(rest[1:], '\n')
	if nl < 0 {
		return "", time.Time{}, errors.New("TZif footer is unterminated")
	}
	return string(rest[1 : 1+nl]), last, nil
}

// parsePosixTZ parses "std offset dst [offset],start[/time],end[/time]" with
// M-form dates. Julian-day forms are refused (no zone in the tzdb uses them
// for a DST rule, and RRULE BYYEARDAY would need care).
func parsePosixTZ(s string) (posixRule, error) {
	var r posixRule
	name := func() (string, error) {
		if strings.HasPrefix(s, "<") {
			i := strings.IndexByte(s, '>')
			if i < 0 {
				return "", errors.New("bad TZ name")
			}
			n := s[1:i]
			s = s[i+1:]
			return n, nil
		}
		i := 0
		for i < len(s) && (s[i] >= 'A' && s[i] <= 'Z' || s[i] >= 'a' && s[i] <= 'z') {
			i++
		}
		if i < 3 {
			return "", errors.New("bad TZ name")
		}
		n := s[:i]
		s = s[i:]
		return n, nil
	}
	// hms parses [+-]hh[:mm[:ss]].
	hms := func() (int, bool) {
		sign := 1
		if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
			if s[0] == '-' {
				sign = -1
			}
			s = s[1:]
		}
		secs, ok := 0, false
		for p := 0; p < 3; p++ {
			i := 0
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if i == 0 {
				break
			}
			v, _ := strconv.Atoi(s[:i])
			secs += v * []int{3600, 60, 1}[p]
			s = s[i:]
			ok = true
			if len(s) == 0 || s[0] != ':' {
				break
			}
			s = s[1:]
		}
		return sign * secs, ok
	}
	var err error
	if r.stdName, err = name(); err != nil {
		return r, err
	}
	off, ok := hms()
	if !ok {
		return r, errors.New("bad TZ offset")
	}
	r.stdOff = -off // POSIX offsets are west of UTC
	if s == "" {
		return r, errors.New("zone has no daylight-saving rule")
	}
	if r.dstName, err = name(); err != nil {
		return r, err
	}
	r.dstOff = r.stdOff + 3600
	if len(s) > 0 && s[0] != ',' {
		off, ok := hms()
		if !ok {
			return r, errors.New("bad TZ dst offset")
		}
		r.dstOff = -off
	}
	parseM := func() (mRule, error) {
		var m mRule
		if !strings.HasPrefix(s, ",M") {
			return m, errors.New("its daylight-saving rule uses a Julian-day date, which is not supported; include a VTIMEZONE in the object")
		}
		s = s[2:]
		parts := make([]int, 3)
		for i := range parts {
			j := 0
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j == 0 {
				return m, errors.New("bad TZ rule date")
			}
			parts[i], _ = strconv.Atoi(s[:j])
			s = s[j:]
			if i < 2 {
				if len(s) == 0 || s[0] != '.' {
					return m, errors.New("bad TZ rule date")
				}
				s = s[1:]
			}
		}
		m.month, m.week, m.day = parts[0], parts[1], parts[2]
		m.secs = 2 * 3600
		if strings.HasPrefix(s, "/") {
			s = s[1:]
			v, ok := hms()
			if !ok {
				return m, errors.New("bad TZ rule time")
			}
			m.secs = v
		}
		return m, nil
	}
	if r.start, err = parseM(); err != nil {
		return r, err
	}
	if r.end, err = parseM(); err != nil {
		return r, err
	}
	if s != "" {
		return r, errors.New("trailing data in TZ string")
	}
	return r, nil
}

// ---- attaching definitions to an object -----------------------------------

// generateVTimezone is GenerateVTimezone; tests swap it to exercise failures.
var generateVTimezone = GenerateVTimezone

// AddMissingVTimezones returns data with a generated VTIMEZONE inserted for
// every IANA TZID the object uses but doesn't define, or data unchanged when
// there is none to add. TZIDs Go can't load (Outlook names, typos) are left
// alone: there is nothing faithful to generate. The insertion is textual —
// the rest of the body keeps its bytes.
func AddMissingVTimezones(data []byte, cal *goical.Calendar) ([]byte, error) {
	defined := map[string]bool{}
	for _, child := range cal.Children {
		if child.Name == goical.CompTimezone {
			if p := child.Props.Get(goical.PropTimezoneID); p != nil {
				defined[p.Value] = true
			}
		}
	}
	covs := map[string]*Coverage{}
	var order []string
	for _, comp := range cal.Children {
		if comp.Name == goical.CompTimezone {
			continue
		}
		collectCoverage(comp, defined, covs, &order)
	}
	if len(order) == 0 {
		return data, nil
	}
	slices.Sort(order)
	var block bytes.Buffer
	for _, tzid := range order {
		vtz, err := generateVTimezone(tzid, *covs[tzid])
		if err != nil {
			return nil, err
		}
		encodeComponent(&block, vtz)
	}
	return insertBeforeFirstComponent(data, block.Bytes()), nil
}

// collectCoverage widens covs with the span each undefined IANA TZID is used
// over in comp: the earliest and latest explicit date, and — for a recurring
// component — through its UNTIL, or unbounded without one.
func collectCoverage(comp *goical.Component, defined map[string]bool, covs map[string]*Coverage, order *[]string) {
	var used []string
	for _, name := range []string{goical.PropDateTimeStart, goical.PropDateTimeEnd, goical.PropDue,
		goical.PropRecurrenceID, goical.PropExceptionDates, goical.PropRecurrenceDates} {
		for _, p := range comp.Props[name] {
			tzid := p.Params.Get(goical.PropTimezoneID)
			if tzid == "" || defined[tzid] {
				continue
			}
			loc, err := time.LoadLocation(tzid)
			if err != nil {
				continue
			}
			c := covs[tzid]
			if c == nil {
				c = &Coverage{}
				covs[tzid] = c
				*order = append(*order, tzid)
			}
			for _, v := range strings.Split(p.Value, ",") {
				t, err := time.ParseInLocation("20060102T150405", v, loc)
				if err != nil {
					continue
				}
				if c.Start.IsZero() || t.Before(c.Start) {
					c.Start = t
				}
				// A day of slack covers an end time or duration past the value.
				if e := t.AddDate(0, 0, 1); e.After(c.End) {
					c.End = e
				}
			}
			used = append(used, tzid)
		}
	}
	if len(used) == 0 {
		return
	}
	if rr := comp.Props.Get(goical.PropRecurrenceRule); rr != nil {
		until := ""
		for _, part := range strings.Split(rr.Value, ";") {
			if k, v, ok := strings.Cut(part, "="); ok && strings.EqualFold(k, "UNTIL") {
				until = v
			}
		}
		for _, tzid := range used {
			c := covs[tzid]
			t, err := time.Parse("20060102T150405Z", until)
			if err != nil {
				t, err = time.Parse("20060102", until)
			}
			if until == "" || err != nil {
				c.Unbounded = true // no UNTIL (or a COUNT we don't evaluate)
			} else if e := t.AddDate(0, 0, 1); e.After(c.End) {
				c.End = e
			}
		}
	}
}

// insertBeforeFirstComponent places block right after the VCALENDAR's own
// properties, using the body's line ending.
func insertBeforeFirstComponent(data, block []byte) []byte {
	nl := "\r\n"
	if !bytes.Contains(data, []byte("\r\n")) {
		nl = "\n"
		block = bytes.ReplaceAll(block, []byte("\r\n"), []byte("\n"))
	}
	lines := strings.SplitAfter(string(data), nl)
	depth := 0
	for i, l := range lines {
		up := strings.ToUpper(strings.TrimRight(l, "\r\n"))
		if strings.HasPrefix(up, "BEGIN:") {
			if depth == 1 {
				return []byte(strings.Join(lines[:i], "") + string(block) + strings.Join(lines[i:], ""))
			}
			depth++
		} else if strings.HasPrefix(up, "END:") {
			depth--
		}
	}
	return data
}
