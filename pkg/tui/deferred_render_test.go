package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type deferredRenderPage struct {
	chat.Page

	views int
}

func (p *deferredRenderPage) UpdateEffects(msg tea.Msg) (chat.Page, chat.Effects) {
	page, effects := p.Page.UpdateEffects(msg)
	p.Page = page
	return p, effects
}

func (p *deferredRenderPage) View() string {
	p.views++
	return p.Page.View()
}

func TestRootBlurDefersViewUntilFocus(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	page := &deferredRenderPage{Page: root.activeTab.chatPage}
	root.activeTab.chatPage = page
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "prefix "))
	before := root.View()
	require.Equal(t, 1, page.views)

	_, _ = root.Update(tea.BlurMsg{})
	require.Equal(t, before, root.View())
	for _, chunk := range []string{"first ", "**second** ", "λ界 third"} {
		_, _ = root.Update(agentruntime.AgentChoice("root", "profile", chunk))
		require.Equal(t, before, root.View(), "streaming must not compose a blurred frame")
	}
	require.Equal(t, 1, page.views)
	require.False(t, root.viewCacheValid, "pending visual changes must survive deferral")

	_, _ = root.Update(tea.FocusMsg{})
	after := root.View()
	require.Contains(t, ansi.Strip(after.Content), "prefix first second λ界 third")
	require.Equal(t, 2, page.views, "focus composes the accumulated changes once")
	require.Equal(t, after, root.View())
	require.Equal(t, 2, page.views)
}

func TestRootBlurKeepsCompletionMetadataLive(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	page := &deferredRenderPage{Page: root.activeTab.chatPage}
	root.activeTab.chatPage = page
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	before := root.View()
	require.NotNil(t, before.ProgressBar)
	_, _ = root.Update(tea.BlurMsg{})

	call := tools.ToolCall{ID: "hidden-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo done"}`}}
	tool := tools.Tool{Name: "shell"}
	for _, event := range []agentruntime.Event{
		agentruntime.ToolCall(call, tool, "root"),
		agentruntime.ToolCallResponse(call.ID, tool, tools.ResultSuccess("done"), "done", "root"),
		agentruntime.AgentChoice("root", "profile", "HIDDEN-COMPLETION"),
		agentruntime.StreamStopped("profile", "root", "normal"),
	} {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: event})
		require.Equal(t, before.Content, root.View().Content)
	}

	require.False(t, root.activeTab.chatPage.IsWorking(), "runtime events still update the page while blurred")
	after := root.View()
	require.Nil(t, after.ProgressBar, "completion must clear taskbar progress without focus")
	require.Equal(t, root.windowTitle(), after.WindowTitle)
	require.NotEqual(t, before.WindowTitle, after.WindowTitle, "completion must clear the title spinner")
	require.Equal(t, 1, page.views)

	_, _ = root.Update(tea.FocusMsg{})
	require.Contains(t, ansi.Strip(root.View().Content), "HIDDEN-COMPLETION")
	require.Equal(t, 2, page.views)
}

func TestRootBlurStartupStillRenders(t *testing.T) {
	for _, blurFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "no focus reporting", true: "blur before first view"}[blurFirst], func(t *testing.T) {
			root, _ := newTestModel(t)
			page := &countingChatPage{text: "first frame"}
			root.activeTab.chatPage = page
			root.activeTab.sessionState = &service.SessionState{}
			root.ar = animation.NewRuntime()
			t.Cleanup(root.ar.Stop)
			root.ready, root.leanMode = true, true
			if blurFirst {
				_, _ = root.Update(tea.BlurMsg{})
			}
			require.Contains(t, root.View().Content, "first frame")
			require.Equal(t, 1, page.views)

			page.text = "next frame"
			_, _ = root.Update(struct{}{})
			if blurFirst {
				require.Contains(t, root.View().Content, "first frame")
				require.Equal(t, 1, page.views)
				_, _ = root.Update(tea.FocusMsg{})
			}
			require.Contains(t, root.View().Content, "next frame")
			require.Equal(t, 2, page.views)
		})
	}
}

func TestRootBlurResizeRefreshesGeometry(t *testing.T) {
	for _, lean := range []bool{false, true} {
		t.Run(map[bool]string{false: "fullscreen", true: "lean"}[lean], func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, 120, 40)
			root.leanMode = lean
			page := &deferredRenderPage{Page: root.activeTab.chatPage}
			root.activeTab.chatPage = page
			_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "before resize"))
			_ = root.View()
			_, _ = root.Update(tea.BlurMsg{})
			_, _ = root.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			after := root.View().Content
			require.Equal(t, 2, page.views, "resize refreshes the cached geometry even while blurred")
			require.Contains(t, []int{29, 30}, len(strings.Split(after, "\n")))
			for _, width := range rootFrameWidths(after) {
				require.Equal(t, 100, width)
			}

			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", " after resize"))
			require.Equal(t, after, root.View().Content, "chunks after resize are deferred again")
			require.Equal(t, 2, page.views)
			_, _ = root.Update(tea.FocusMsg{})
			require.Contains(t, ansi.Strip(root.View().Content), "before resize after resize")
			require.Equal(t, 3, page.views)
		})
	}
}

func TestRootBlurFinalLeanFrameIsCurrent(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	root.leanMode = true
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	_ = root.View()
	_, _ = root.Update(tea.BlurMsg{})
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "FINAL-LEAN-FRAME"))
	require.NotContains(t, root.View().Content, "FINAL-LEAN-FRAME")

	_, cmd := root.Update(messages.ExitAfterFirstResponseMsg{})
	require.True(t, hasMsg[tea.QuitMsg](collectMsgs(cmd)))
	require.Contains(t, ansi.Strip(root.View().Content), "FINAL-LEAN-FRAME")
}

func TestRootBlurDoesNotHideErrors(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	_, _ = root.Update(tea.BlurMsg{})
	_, _ = root.Update(errors.New("hidden fatal error"))
	assert.Contains(t, ansi.Strip(root.View().Content), "hidden fatal error")
}

func TestActualProgramBlurSuppressesStreamWrites(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "prefix "))
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	writer := &wallClockCountingWriter{}
	program := startStreamingMotionProgram(t, model, tea.WithOutput(writer))
	program.Send(tea.BlurMsg{})
	programAck(t, program)
	waitForProgramQuiescence(t, model, writer)
	before := programFrame(t, program)
	baselineWrites := writer.writes.Load()
	baselineCompositions := model.compositions.Load()

	for range 100 {
		program.Send(agentruntime.AgentChoice("root", "profile", "hidden "))
	}
	program.Send(agentruntime.AgentChoice("root", "profile", "FOCUS-CATCHUP"))
	programAck(t, program)
	waitForProgramQuiescence(t, model, writer)
	require.Equal(t, uint64(101), model.chunks.Load())
	require.Equal(t, before, programFrame(t, program))
	require.Equal(t, baselineWrites, writer.writes.Load(), "blurred chunks must not reach the terminal")
	require.Equal(t, baselineCompositions, model.compositions.Load(), "blurred chunks must not compose frames")

	program.Send(tea.FocusMsg{})
	programAck(t, program)
	require.Contains(t, ansi.Strip(programFrame(t, program)), "FOCUS-CATCHUP")
}
