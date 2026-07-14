package chat

import "regexp"

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripANSI removes SGR escapes so tests can assert on visible text.
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }
