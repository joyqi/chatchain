package chat

import "chatchain/internal/textwidth"

// displayWidth and runeWidth are thin aliases over the shared width rulers in
// internal/textwidth (uniseg for strings, go-runewidth for single runes) —
// kept as package-local names so the many call sites across chat read short.
func displayWidth(s string) int { return textwidth.StringWidth(s) }
func runeWidth(r rune) int      { return textwidth.RuneWidth(r) }
