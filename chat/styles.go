package chat

import "github.com/fatih/color"

var (
	UserStyle = color.New(color.FgCyan, color.Bold)
	// UserBlockStyle renders a sent user message as a full-width block: reverse
	// video swaps fg/bg with no explicit color, so the terminal's default text
	// color becomes the line background and its background becomes the text color.
	UserBlockStyle = color.New(color.ReverseVideo)
	ErrorStyle     = color.New(color.FgRed)
	BoldStyle      = color.New(color.Bold)
	DimStyle       = color.New(color.Faint)
	CodeStyle      = color.New(color.FgCyan)
	CodeBlockStyle = color.New(color.FgGreen)
	YellowStyle    = color.New(color.FgYellow)
	ItalicStyle    = color.New(color.Italic)
	UnderlineStyle = color.New(color.Underline)
	LinkStyle      = color.New(color.FgCyan, color.Underline)

	// Input-composer status-bar field styles: each field gets its own faint hue
	// so they read as distinct at a glance while staying muted (see the composer
	// status line). color.Faint dims the hue; NoColor drops both.
	StatusModelStyle = color.New(color.FgCyan, color.Faint)  // model name
	StatusCtxStyle   = color.New(color.FgGreen, color.Faint) // context-window usage
	StatusSepStyle   = color.New(color.Faint)                // " · " field separator
)
