// Package guard treats telemetry values as untrusted. Log fields such as user
// agents, URLs and file names are attacker-controlled; when they flow into an
// LLM prompt or an agent's tool result they can carry instructions. Guard
// detects likely injection content and produces safe, truncated renderings.
//
// Detection is defense in depth. The primary control is that an agent's
// authority comes from the policy layer, never from data.
package guard

import (
	"regexp"
	"strings"
	"unicode"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all |any )?(previous|prior|above) (instructions|prompts?)`),
	regexp.MustCompile(`(?i)(system|developer) (note|prompt|message|instruction)s?\b`),
	regexp.MustCompile(`(?i)\b(you are|act as) (an? )?(ai|assistant|analyst|model)\b`),
	regexp.MustCompile(`(?i)\bnote to (the )?(ai|llm|assistant|analyst|model)\b`),
	regexp.MustCompile(`(?i)\b(classify|mark|report|treat) .{0,40}\bas (benign|safe|clean|false positive)\b`),
	regexp.MustCompile(`(?i)<\/?(system|assistant|user|instructions?)>`),
	regexp.MustCompile(`(?i)\bdisregard\b.{0,30}\b(rules|instructions|policy)\b`),
}

// Suspicious reports whether a value looks like an instruction aimed at an AI.
func Suspicious(s string) bool {
	for _, p := range patterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

// Sanitize strips control characters and truncates to max runes.
func Sanitize(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// Redacted is what an agent sees instead of a suspicious value.
const Redacted = "[withheld: value matched prompt-injection heuristics; available to human analysts]"

// ForAgent returns the agent-facing rendering of a value and whether it was
// flagged.
func ForAgent(s string) (string, bool) {
	if Suspicious(s) {
		return Redacted, true
	}
	return Sanitize(s, 200), false
}
