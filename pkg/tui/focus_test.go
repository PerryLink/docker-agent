package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestViewsRequestFocusWithoutKeyboardEventTypes(t *testing.T) {
	t.Parallel()
	for _, lean := range []bool{false, true} {
		view := toFullscreenView("content", "title", true, lean)
		assert.True(t, view.ReportFocus)
		assert.False(t, view.KeyboardEnhancements.ReportEventTypes, "do not revive the VS Code AZERTY regression")
	}
}

func TestTmuxFocusHidden(t *testing.T) {
	t.Parallel()
	tty, err := os.Create(filepath.Join(t.TempDir(), "tty"))
	require.NoError(t, err)
	defer tty.Close()
	otherTTY := filepath.Join(t.TempDir(), "other-tty")
	require.NoError(t, os.WriteFile(otherTTY, nil, 0o600))
	for _, tt := range []struct {
		name, output string
		hidden       bool
	}{
		{"detached", "1 1 0", true},
		{"inactive pane", "1 0 1", true},
		{"attached", "on 1 1", false},
		{"linked window", "on 1 2", false},
		{"focus reporting disabled", "0 1 0", false},
		{"missing format", "on 1", false},
		{"empty", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.hidden, tmuxFocusHidden(tt.output+" "+tty.Name()+"\n", tty))
		})
	}
	assert.False(t, tmuxFocusHidden("on 1 0 "+otherTTY, tty), "inherited tmux env must not hide another terminal")
	assert.False(t, tmuxFocusHidden("on 1 0 /missing/tty", tty))
}

func TestInitialFocusProbeWaitsForTerminalFence(t *testing.T) {
	t.Setenv("TMUX", "/unused,1,0")
	t.Setenv("TMUX_PANE", "%0")
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntime()
	cmd := root.initialFocusCmd()
	require.NotNil(t, cmd)
	raw, ok := cmd().(tea.RawMsg)
	require.True(t, ok)
	assert.Equal(t, ansi.SetModeFocusEvent+ansi.RequestCursorPositionReport, raw.Msg)
	assert.True(t, root.tmuxFocusPending)

	calls := 0
	root.tmuxFocusProbe = func() tea.Msg {
		calls++
		return initialFocusMsg{hidden: true}
	}
	_, cmd = root.Update(tea.CursorPositionMsg{})
	require.NotNil(t, cmd)
	assert.Zero(t, calls, "probe must run outside the update loop")
	_, _ = root.Update(cmd())
	assert.Equal(t, 1, calls)
	sub := root.ar.Subscribe()
	assert.Nil(t, sub.Start(), "detached startup needs no prior BlurMsg")
	sub.Stop()
	_, cmd = root.Update(tea.CursorPositionMsg{})
	assert.Nil(t, cmd, "only probe once")
}

func TestInitialFocusProbeIsWiredIntoRootInit(t *testing.T) {
	t.Setenv("TMUX", "/unused,1,0")
	t.Setenv("TMUX_PANE", "%0")
	root, _, _ := wallClockRoot(t, 120, 40)
	// The harness called the real Init but deliberately did not execute its commands.
	assert.True(t, root.tmuxFocusPending)
	require.NotNil(t, root.tmuxFocusProbe)
}

func TestTmuxProbeFailureLeavesAnimationsEnabled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	assert.False(t, tmuxPaneHidden(t.Context(), "%0", os.Stdout))
}

func TestNoTmuxKeepsStartupAnimations(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntime()
	assert.Nil(t, root.initialFocusCmd())
	sub := root.ar.Subscribe()
	require.NotNil(t, sub.Start(), "terminals that never send focus must still animate")
	sub.Stop()
}

func TestRealFocusEventsOverrideStaleStartupProbe(t *testing.T) {
	for _, event := range []tea.Msg{tea.FocusMsg{}, tea.BlurMsg{}} {
		root, _ := newTestModel(t)
		root.ar = animation.NewRuntime()
		_, _ = root.Update(event)
		_, _ = root.Update(initialFocusMsg{hidden: true})
		sub := root.ar.Subscribe()
		cmd := sub.Start()
		if _, focused := event.(tea.FocusMsg); focused {
			assert.NotNil(t, cmd, "late snapshot cannot pause a newly attached pane")
		} else {
			assert.Nil(t, cmd)
		}
		sub.Stop()
	}
}

func TestRootFocusPausesTicksWithoutDroppingRuntimeEvents(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(tea.BlurMsg{})
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	require.True(t, root.activeTab.chatPage.IsWorking())
	require.True(t, root.ar.HasActive(), "working components keep their subscriptions")
	assert.Nil(t, root.ar.Continue())
	assert.Nil(t, root.ar.EnsureRunning())
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", "hidden stream still progresses")})
	assert.Contains(t, ansi.Strip(root.View().Content), "hidden stream still progresses")
	assert.Zero(t, root.ar.Now())

	_, cmd := root.Update(tea.FocusMsg{})
	msgs := collectMsgs(cmd)
	require.True(t, hasMsg[animation.TickMsg](msgs))
	for _, msg := range msgs {
		_, _ = root.Update(msg)
	}
	assert.Positive(t, root.ar.Now())
	_, cmd = root.Update(tea.FocusMsg{})
	assert.Nil(t, cmd, "duplicate focus must not replace the live chain")
	root.ar.Stop()
}

func TestRootBlurRejectsQueuedTickAndPreservesViewCache(t *testing.T) {
	root, _ := newTestModel(t)
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	page := &countingChatPage{text: "stable", dirtyTick: true}
	root.activeTab.chatPage = page
	sub := root.ar.Subscribe()
	queued := sub.Start()
	_, _ = root.Update(tea.BlurMsg{})
	root.viewCacheValid = true
	_, cmd := root.Update(queued())
	assert.Nil(t, cmd)
	assert.True(t, root.viewCacheValid, "a stale tick cannot trigger composition")
	assert.Zero(t, root.ar.Now())
	sub.Stop()
}
