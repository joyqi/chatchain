package chat

import "github.com/fatih/color"

var (
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
	UnderlineStyle = color.New(color.Underline)
)
