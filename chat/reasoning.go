package chat

// reasoningSymbol marks a finished reasoning block: a dim diamond shown in the
// collapsed summary (the live header is an animated spinner instead).
const reasoningSymbol = "◇"

func dim(s string) string {
	return DimStyle.Sprint(s)
}
