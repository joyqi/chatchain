package chat

import "github.com/charmbracelet/lipgloss"

var (
	UserStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	AssistantStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
	ErrorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)
