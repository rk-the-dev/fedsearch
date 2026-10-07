package ir

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Now is the clock used to resolve relative times. Tests and deterministic
// demos replace it.
var Now = func() time.Time { return time.Now().UTC() }

var relRe = regexp.MustCompile(`^now(?:\s*([+-])\s*(\d+)\s*([smhdw]))?$`)

// ParseTime accepts RFC3339, YYYY-MM-DD, or now[+-]N{s,m,h,d,w}.
func ParseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if m := relRe.FindStringSubmatch(s); m != nil {
		if m[1] == "" {
			return now.UTC(), nil
		}
		n, _ := strconv.Atoi(m[2])
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[3]]
		d := time.Duration(n) * unit
		if m[1] == "-" {
			d = -d
		}
		return now.UTC().Add(d), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.ToUpper(s)); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q (use RFC3339, YYYY-MM-DD or now-30d)", s)
}

func (t *TimeRange) UnmarshalJSON(b []byte) error {
	var raw struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	now := Now()
	if raw.To == "" {
		raw.To = "now"
	}
	from, err := ParseTime(raw.From, now)
	if err != nil {
		return fmt.Errorf("time.from: %w", err)
	}
	to, err := ParseTime(raw.To, now)
	if err != nil {
		return fmt.Errorf("time.to: %w", err)
	}
	t.From, t.To = from.Truncate(time.Millisecond), to.Truncate(time.Millisecond)
	return nil
}

func (t TimeRange) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"from": t.From.UTC().Format(time.RFC3339Nano),
		"to":   t.To.UTC().Format(time.RFC3339Nano),
	})
}
