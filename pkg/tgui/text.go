package tgui

import "unicode/utf8"

// TruncRunes returns s truncated to at most n runes.
// It appends an ellipsis "…" when truncated.
func TruncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Single-pass implementation:
	//  - remember the byte index after the n-th rune
	//  - if there is an (n+1)-th rune, truncate + ellipsis
	count := 0
	cut := 0
	for i, r := range s {
		count++
		if count == n {
			cut = i + utf8.RuneLen(r)
			continue
		}
		if count > n {
			if cut <= 0 {
				cut = i
			}
			return s[:cut] + "…"
		}
	}
	return s
}
