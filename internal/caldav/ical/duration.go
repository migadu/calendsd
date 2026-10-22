package ical

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AddDuration adds an iCalendar DURATION value to t. Weeks and days are
// nominal (RFC 5545 §3.3.6): they advance the calendar date in t's location
// and keep the wall-clock time across a DST change, so P1D from 10:00 ends
// at 10:00 the next day even when that day has 23 or 25 hours. Hours,
// minutes and seconds are exact.
func AddDuration(t time.Time, value string) (time.Time, error) {
	days, exact, err := parseDuration(value)
	if err != nil {
		return time.Time{}, err
	}
	return t.AddDate(0, 0, days).Add(exact), nil
}

// parseDuration splits a dur-value into its nominal days (weeks included)
// and its exact time part, both carrying the sign.
func parseDuration(value string) (days int, exact time.Duration, err error) {
	invalid := fmt.Errorf("invalid DURATION %q", value)
	s, sign := value, 1
	switch {
	case strings.HasPrefix(s, "-"):
		s, sign = s[1:], -1
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if !strings.HasPrefix(s, "P") {
		return 0, 0, invalid
	}
	s = s[1:]
	inTime, seen := false, false
	for s != "" {
		if s[0] == 'T' {
			if inTime {
				return 0, 0, invalid
			}
			inTime, s = true, s[1:]
			continue
		}
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i == len(s) {
			return 0, 0, invalid
		}
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, 0, invalid
		}
		unit := s[i]
		s, seen = s[i+1:], true
		switch {
		case !inTime && unit == 'W':
			days += 7 * n
		case !inTime && unit == 'D':
			days += n
		case inTime && unit == 'H':
			exact += time.Duration(n) * time.Hour
		case inTime && unit == 'M':
			exact += time.Duration(n) * time.Minute
		case inTime && unit == 'S':
			exact += time.Duration(n) * time.Second
		default:
			return 0, 0, invalid
		}
	}
	if !seen {
		return 0, 0, invalid
	}
	return sign * days, time.Duration(sign) * exact, nil
}
