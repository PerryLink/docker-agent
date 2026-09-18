//go:build !windows

package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type focusTestState struct {
	Elapsed       time.Duration
	Probed        bool
	Hidden        bool
	Focus, Blur   int
	Subscriptions int32
}

type focusTestModel struct {
	root  *appModel
	state focusTestState
	path  string
	sub   animation.Subscription
	extra animation.Subscription
}

func (m *focusTestModel) Init() tea.Cmd {
	return tea.Batch(m.root.initialFocusCmd(), m.sub.Start())
}

func (m *focusTestModel) View() tea.View { return m.root.View() }

func (m *focusTestModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var extra tea.Cmd
	switch msg := msg.(type) {
	case initialFocusMsg:
		m.state.Probed, m.state.Hidden = true, msg.hidden
	case tea.FocusMsg:
		m.state.Focus++
	case tea.BlurMsg:
		m.state.Blur++
	case tea.KeyPressMsg:
		switch msg.Text {
		case "w":
			extra = m.extra.Start()
		case "q":
			return m, tea.Quit
		}
	}
	_, cmd := m.root.Update(msg)
	m.state.Elapsed = m.root.ar.Now()
	m.state.Subscriptions = m.root.ar.ActiveCount()
	data, err := json.Marshal(m.state)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(m.path+".tmp", data, 0o600); err != nil {
		panic(err)
	}
	if err := os.Rename(m.path+".tmp", m.path); err != nil {
		panic(err)
	}
	return m, tea.Batch(cmd, extra)
}

// The helper runs the production root, renderer and input parser inside a real
// tmux pane. Only its chat content and instrumentation are test-specific.
func TestTmuxFocusHelper(t *testing.T) {
	path := os.Getenv("DOCKER_AGENT_TEST_FOCUS_STATE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	root, _, _ := wallClockRoot(t, 80, 24)
	defer root.ar.Stop()
	root.activeTab.chatPage = &countingChatPage{text: "focus integration", dirtyTick: true}
	root.activeTab.sessionState = &service.SessionState{}
	root.ready, root.leanMode, root.appName = true, true, "focus-test"
	model := &focusTestModel{root: root, path: path, sub: root.ar.Subscribe(), extra: root.ar.Subscribe()}
	_, err := tea.NewProgram(model, tea.WithContext(t.Context())).Run()
	require.NoError(t, err)
}

func TestTmuxDetachedAnimationLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// macOS sun_path is shorter than a t.TempDir path containing this test name.
	dir, err := os.MkdirTemp("", "focus") //nolint:forbidigo,usetesting // private short Unix socket path
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "tmux.sock")
	base := []string{"-S", socket, "-f", "/dev/null"}
	tmux := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "tmux", append(base, args...)...).CombinedOutput()
		require.NoError(t, err, "tmux %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	tmux("new-session", "-d", "-s", "focus-test", "exec cat")
	t.Cleanup(func() {
		// Only ever target this test's private server, never the user's tmux.
		_ = exec.Command("tmux", "-S", socket, "kill-server").Run() //nolint:noctx // cleanup must outlive the test context
	})
	tmux("set-option", "-s", "focus-events", "on")
	statePath := filepath.Join(dir, "state")
	executable, err := os.Executable()
	require.NoError(t, err)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := "exec env DOCKER_AGENT_TEST_FOCUS_STATE=" + quote(statePath) + " " + quote(executable) + " -test.run=^TestTmuxFocusHelper$ -test.timeout=30s"
	tmux("respawn-pane", "-k", "-t", "focus-test", command)

	readState := func() focusTestState {
		t.Helper()
		data, _ := os.ReadFile(statePath)
		var state focusTestState
		_ = json.Unmarshal(data, &state)
		return state
	}
	require.Eventually(t, func() bool { return readState().Probed }, 10*time.Second, 20*time.Millisecond, "startup snapshot was not applied")
	require.True(t, readState().Hidden, "detached startup must be detected even without an initial blur: %s", tmux("display-message", "-p", "-t", "focus-test", tmuxFocusFormat))
	assertStopped := func() {
		t.Helper()
		before := readState().Elapsed
		assert.Never(t, func() bool { return readState().Elapsed != before }, 400*time.Millisecond, 20*time.Millisecond, "hidden pane accepted animation ticks")
	}
	assertStopped()

	tmux("send-keys", "-t", "focus-test", "w")
	require.Eventually(t, func() bool { return readState().Subscriptions == 2 }, 5*time.Second, 20*time.Millisecond, "hidden root did not process new work")
	assertStopped()

	attach := exec.CommandContext(t.Context(), "tmux", append(base, "attach-session", "-t", "focus-test")...)
	attach.Env = append(os.Environ(), "TMUX=", "TERM=xterm-256color")
	terminal, err := pty.StartWithSize(attach, &pty.Winsize{Rows: 24, Cols: 80})
	require.NoError(t, err)
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, terminal); close(drained) }()
	attachDone := make(chan error, 1)
	go func() { attachDone <- attach.Wait() }()
	t.Cleanup(func() {
		_ = terminal.Close()
		<-drained
		select {
		case <-attachDone:
		case <-time.After(3 * time.Second):
			_ = attach.Process.Kill()
			<-attachDone
		}
	})
	before := readState().Elapsed
	require.Eventually(t, func() bool { return readState().Focus > 0 && readState().Elapsed > before }, 5*time.Second, 20*time.Millisecond, "attach did not resume animations")
	tmux("detach-client", "-s", "focus-test")
	require.Eventually(t, func() bool { return readState().Blur > 0 }, 5*time.Second, 20*time.Millisecond, "detach did not reach root as BlurMsg")
	assertStopped()
	t.Logf("tmux lifecycle: %s", fmt.Sprint(readState()))
	tmux("send-keys", "-t", "focus-test", "q")
}
