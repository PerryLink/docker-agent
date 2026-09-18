package tui

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type initialFocusMsg struct{ hidden bool }

func (m *appModel) initialFocusCmd() tea.Cmd {
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || pane == "" {
		return nil
	}
	m.tmuxFocusProbe = func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx(), time.Second)
		defer cancel()
		return initialFocusMsg{hidden: tmuxPaneHidden(ctx, pane, os.Stdout)}
	}
	m.tmuxFocusPending = true
	// tmux 3.6 sends no initial blur. A cursor reply fences enabling reports
	// before the snapshot; unlike DECRQM 1004, it also works on older tmux.
	return tea.Raw(ansi.SetModeFocusEvent + ansi.RequestCursorPositionReport)
}

const tmuxFocusFormat = "#{focus-events} #{pane_active} #{window_active_clients} #{pane_tty}"

func tmuxPaneHidden(ctx context.Context, pane string, output *os.File) bool {
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", pane, tmuxFocusFormat).Output()
	if err != nil {
		return false
	}
	return tmuxFocusHidden(string(out), output)
}

func tmuxFocusHidden(out string, output *os.File) bool {
	fields := strings.Fields(out)
	// Without focus-events we cannot resume on attach. Unknown/failed probes
	// likewise leave animations enabled rather than freezing a visible UI.
	if len(fields) != 4 || (fields[0] != "1" && fields[0] != "on") || (fields[1] != "0" && fields[2] != "0") {
		return false
	}
	// GUI terminals can inherit TMUX from a shell in an unrelated pane.
	paneTTY, err := os.Stat(fields[3])
	if err != nil {
		return false
	}
	ourTTY, err := output.Stat()
	return err == nil && os.SameFile(paneTTY, ourTTY)
}
