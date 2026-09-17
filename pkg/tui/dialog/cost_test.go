package dialog

import (
	"image/color"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestNewCostDialog(t *testing.T) {
	t.Parallel()

	sess := session.New()

	dialog := NewCostDialog(sess)

	require.NotNil(t, dialog)
}

func TestCostDialogView(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add some messages with usage info
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 500,
			},
			Cost: 0.005,
		},
	})

	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "World",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:       800,
				OutputTokens:      300,
				CachedInputTokens: 200,
			},
			Cost: 0.003,
		},
	})

	dialog := NewCostDialog(sess)
	// Set a large enough window size
	dialog.SetSize(100, 50)
	view := dialog.View()

	// Check that the view contains expected content
	// The title may be split across lines due to narrow width
	assert.Contains(t, view, "Session Cost")
	assert.Contains(t, view, "Total")
	assert.Contains(t, view, "By Agent")
	assert.Contains(t, view, "By Model")
	assert.Contains(t, view, "gpt-4o")
	assert.Contains(t, view, "tokens:")           // total token count line
	assert.Contains(t, view, "messages:")         // message count in header
	assert.Contains(t, view, "avg cost/message:") // average cost per message
}

// TestCostDialogByAgentAggregation verifies the By Agent section aggregates
// the exact per-message records by agent name, sorts descending by cost,
// keeps records without an agent honestly unattributed, and books compaction
// spend in its own bucket instead of crediting an agent for it.
func TestCostDialogByAgentAggregation(t *testing.T) {
	t.Parallel()

	sess := session.New()
	addMsg := func(agent string, cost float64, usage chat.Usage) {
		sess.AddMessage(&session.Message{
			AgentName: agent,
			Message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "msg",
				Model:   "gpt-4o",
				Usage:   &usage,
				Cost:    cost,
			},
		})
	}
	addMsg("root", 0.02, chat.Usage{InputTokens: 1000, OutputTokens: 200})
	addMsg("developer", 0.10, chat.Usage{InputTokens: 4000, OutputTokens: 800})
	addMsg("root", 0.03, chat.Usage{InputTokens: 1500, OutputTokens: 300})
	addMsg("", 0.01, chat.Usage{InputTokens: 500, OutputTokens: 100})
	sess.Messages = append(sess.Messages, session.Item{Summary: "compacted", Cost: 0.004})

	data := (&costDialog{session: sess}).gatherCostData()

	require.Len(t, data.agents, 4)
	assert.Equal(t, "developer", data.agents[0].label, "sorted descending by cost")
	assert.InDelta(t, 0.10, data.agents[0].cost, 1e-9)
	assert.Equal(t, "root", data.agents[1].label)
	assert.InDelta(t, 0.05, data.agents[1].cost, 1e-9, "root's two messages add up")
	assert.Equal(t, int64(2500), data.agents[1].InputTokens, "tokens aggregate per agent")
	assert.Equal(t, "(unattributed)", data.agents[2].label, "agent-less records stay unattributed")
	assert.InDelta(t, 0.01, data.agents[2].cost, 1e-9)
	assert.Equal(t, "compaction", data.agents[3].label, "compaction spend keeps its own bucket")
	assert.InDelta(t, 0.004, data.agents[3].cost, 1e-9)
}

// TestCostDialogByAgentRendersBeforeByModel verifies the styled view renders
// the By Agent section between Total and By Model, and that the clipboard
// plain text carries it too.
func TestCostDialogByAgentRendersBeforeByModel(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage:   &chat.Usage{InputTokens: 1000, OutputTokens: 500},
			Cost:    0.005,
		},
	})

	d := NewCostDialog(sess)
	d.SetSize(100, 50)
	view := d.View()
	byAgent := strings.Index(view, "By Agent")
	byModel := strings.Index(view, "By Model")
	require.Positive(t, byAgent, "By Agent renders")
	require.Positive(t, byModel, "By Model renders")
	assert.Less(t, byAgent, byModel, "By Agent renders before By Model")

	plain := d.(*costDialog).renderPlainText()
	assert.Contains(t, plain, "By Agent")
	assert.Contains(t, plain, "root")
	assert.Contains(t, plain, "By Model")
	assert.Contains(t, plain, "By Message")
	assert.Less(t, strings.Index(plain, "By Agent"), strings.Index(plain, "By Model"),
		"clipboard output keeps the section order")
}

func TestCostDialogWithToolCalls(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add message with tool calls
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Let me help you",
			Model:   "claude-sonnet-4-0",
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}},
			},
			Usage: &chat.Usage{
				InputTokens:  2000,
				OutputTokens: 100,
			},
			Cost: 0.01,
		},
	})

	dialog := NewCostDialog(sess)
	// Set a large enough window size
	dialog.SetSize(100, 50)
	view := dialog.View()

	// Model name may be split across lines
	assert.Contains(t, view, "claude")
	assert.Contains(t, view, "$0.01")
}

func TestCostDialogWithReasoningTokens(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Thought deeply",
			Model:   "o1",
			Usage: &chat.Usage{
				InputTokens:     500,
				OutputTokens:    200,
				ReasoningTokens: 1500,
			},
			Cost: 0.01,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "reasoning:")
	assert.Contains(t, view, "1.5K")
}

func TestCostDialogAvgCostPerToken(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 1000,
			},
			Cost: 0.10,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	// 0.10 / 2000 * 1000 = 0.05 per 1K tokens
	assert.Contains(t, view, "avg cost/1K tokens:")
}

func TestCostDialogModelPercentage(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Expensive",
			Model:   "gpt-4o",
			Usage:   &chat.Usage{InputTokens: 1000, OutputTokens: 500},
			Cost:    0.75,
		},
	})
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Cheap",
			Model:   "gpt-4o-mini",
			Usage:   &chat.Usage{InputTokens: 100, OutputTokens: 50},
			Cost:    0.25,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(120, 50)
	view := dialog.View()

	// gpt-4o should show 75%, gpt-4o-mini 25%
	assert.Contains(t, view, "75%")
	assert.Contains(t, view, "25%")
}

func TestCostDialogCachedCountAlignedAfterPercentage(t *testing.T) {
	t.Parallel()

	dialog := &costDialog{}
	usage := chat.Usage{InputTokens: 100, CachedInputTokens: 100}
	rows := []totalUsage{
		{Usage: usage, label: "short", cost: 0.04},
		{Usage: usage, label: "long-model-name", cost: 0.14},
	}
	labelWidth := usageLabelWidth(rows)
	singleDigit := dialog.renderUsageLine(rows[0], 1, labelWidth, false)
	doubleDigit := dialog.renderUsageLine(rows[1], 1, labelWidth, false)

	singleDigitPrefix, _, singleDigitFound := strings.Cut(singleDigit, "cached:")
	doubleDigitPrefix, _, doubleDigitFound := strings.Cut(doubleDigit, "cached:")
	require.True(t, singleDigitFound)
	require.True(t, doubleDigitFound)
	assert.Equal(t, lipgloss.Width(singleDigitPrefix), lipgloss.Width(doubleDigitPrefix))
}

func TestCostDialogTotalStatsAligned(t *testing.T) {
	t.Parallel()

	stats := []stat{{label: "in:", value: "100"}, {label: "avg cost/message:", value: "$0.01"}}
	labelWidth := statLabelWidth(stats)
	shortLabel := styledStat(stats[0], labelWidth)
	longLabel := styledStat(stats[1], labelWidth)

	assert.Equal(t, strings.Index(shortLabel, "100"), strings.Index(longLabel, "$0.01"))
}

func TestCostDialogMessageCacheMiss(t *testing.T) {
	t.Parallel()

	line := (&costDialog{}).renderUsageLine(totalUsage{
		Usage: chat.Usage{InputTokens: 100},
		label: "#1",
		model: "gpt-4o",
		cost:  0.01,
	}, 0.01, 2, true)

	assert.Contains(t, line, styles.WarningStyle.Render("cache miss"))
}

func TestCostPercentageStyle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		percentage float64
		want       color.Color
	}{
		{name: "neutral", percentage: 0, want: styles.TextSecondary},
		{name: "warning", percentage: 35, want: styles.Warning},
		{name: "error", percentage: 100, want: styles.Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotR, gotG, gotB := styles.ColorToRGB(costPercentageStyle(tt.percentage).GetForeground())
			wantR, wantG, wantB := styles.ColorToRGB(tt.want)
			assert.InDelta(t, wantR, gotR, 0.01)
			assert.InDelta(t, wantG, gotG, 0.01)
			assert.InDelta(t, wantB, gotB, 0.01)
		})
	}
}

func TestCostDialogCacheHitRate(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Cached result",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:       300,
				CachedInputTokens: 700,
				OutputTokens:      100,
			},
			Cost: 0.01,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(130, 50)
	view := dialog.View()

	// 700 cached out of 1000 total input = 70% hit rate
	assert.Contains(t, view, "cache hit rate:")
	assert.Contains(t, view, "70%")

	// By Model line should also show cached token count
	assert.Contains(t, view, "cached:")
}

func TestCostDialogCacheHitRateWithCacheWriteTokens(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Cached result with write tokens",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:       300,
				CachedInputTokens: 700,
				CacheWriteTokens:  200,
				OutputTokens:      100,
			},
			Cost: 0.01,
		},
	})

	data := (&costDialog{session: sess}).gatherCostData()
	stats := data.totalStats()

	// Cache hit rate should be 700/(700+300) = 70%, NOT 700/(700+300+200) = 58%.
	// CacheWriteTokens must NOT be included in the denominator.
	var cacheHitRate string
	for _, s := range stats {
		if s.label == "cache hit rate:" {
			cacheHitRate = s.value
		}
	}
	assert.Equal(t, "70%", cacheHitRate)
}

func TestCostDialogEmptySession(t *testing.T) {
	t.Parallel()

	sess := session.New()

	dialog := NewCostDialog(sess)
	// Set a large enough window size
	dialog.SetSize(100, 50)
	view := dialog.View()

	// Should still render without errors
	assert.Contains(t, view, "Session Cost")
	assert.Contains(t, view, "Total")
	assert.Contains(t, view, "$0.00") // Zero cost
}

func TestCostDialogWithCompactionCost(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add a regular message with usage
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 500,
			},
			Cost: 0.005,
		},
	})

	// Add a compaction summary item with cost (simulates what session_compaction.go does)
	sess.Messages = append(sess.Messages, session.Item{
		Summary: "This is a session summary after compaction.",
		Cost:    0.003,
	})

	data := (&costDialog{session: sess}).gatherCostData()

	// Total cost should include both the message cost and the compaction cost
	assert.InDelta(t, 0.008, data.total.cost, 0.0001)

	// There should be 2 entries in the per-message breakdown:
	// one for the assistant message and one for compaction
	require.Len(t, data.messages, 2)
	assert.InDelta(t, 0.005, data.messages[0].cost, 0.0001)
	assert.Equal(t, "compaction", data.messages[1].label)
	assert.InDelta(t, 0.003, data.messages[1].cost, 0.0001)
}

// TestCostDialogCompactionModelAttribution verifies that a summary item
// carrying model + usage (a dedicated compaction model) shows up in the
// By Model breakdown instead of silently vanishing from it.
func TestCostDialogCompactionModelAttribution(t *testing.T) {
	t.Parallel()

	sess := session.New()

	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage:   &chat.Usage{InputTokens: 1000, OutputTokens: 500},
			Cost:    0.005,
		},
	})

	// Summary generated by a dedicated (cheaper) compaction model.
	sess.Messages = append(sess.Messages, session.Item{
		Summary: "Session summary.",
		Cost:    0.003,
		Model:   "gpt-4o-mini",
		Usage:   &chat.Usage{InputTokens: 800, OutputTokens: 200},
	})

	data := (&costDialog{session: sess}).gatherCostData()

	require.Len(t, data.models, 2, "the compaction model must get its own By Model bucket")
	models := map[string]totalUsage{}
	for _, m := range data.models {
		models[m.label] = m
	}
	compactionModel, ok := models["gpt-4o-mini"]
	require.True(t, ok)
	assert.InDelta(t, 0.003, compactionModel.cost, 0.0001)
	assert.Equal(t, int64(800), compactionModel.InputTokens)
	assert.Equal(t, int64(200), compactionModel.OutputTokens)

	// The compaction usage also counts toward the total token tallies.
	assert.Equal(t, int64(1800), data.total.InputTokens)
	assert.Equal(t, int64(700), data.total.OutputTokens)
	assert.InDelta(t, 0.008, data.total.cost, 0.0001)

	// Legacy summary items without a recorded model stay out of By Model.
	sess.Messages = append(sess.Messages, session.Item{Summary: "Old summary.", Cost: 0.001})
	data = (&costDialog{session: sess}).gatherCostData()
	assert.Len(t, data.models, 2)
	assert.InDelta(t, 0.009, data.total.cost, 0.0001)
}

func TestCostDialogCompactionCostRendersInView(t *testing.T) {
	t.Parallel()

	sess := session.New()

	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 500,
			},
			Cost: 0.005,
		},
	})

	sess.Messages = append(sess.Messages, session.Item{
		Summary: "Session summary.",
		Cost:    0.002,
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "compaction")
	assert.Contains(t, view, "$0.0070") // total: 0.005 + 0.002
}

func TestCostDialogWithSubSessions(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add a parent message with usage
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Let me create a sub-session",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 200,
			},
			Cost: 0.005,
		},
	})

	// Create a sub-session with its own messages
	subSess := session.New()
	subSess.AddMessage(&session.Message{
		AgentName: "sub-agent",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Working on it",
			Model:   "gpt-4o-mini",
			Usage: &chat.Usage{
				InputTokens:  500,
				OutputTokens: 100,
			},
			Cost: 0.001,
		},
	})
	subSess.AddMessage(&session.Message{
		AgentName: "sub-agent",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Done!",
			Model:   "gpt-4o-mini",
			Usage: &chat.Usage{
				InputTokens:  600,
				OutputTokens: 150,
			},
			Cost: 0.002,
		},
	})

	sess.AddSubSession(subSess)

	// Gather cost data
	data := (&costDialog{session: sess}).gatherCostData()

	// Total cost should include parent + sub-session messages
	assert.InDelta(t, 0.008, data.total.cost, 0.0001)

	// Messages should include: parent msg, sub-session start marker, 2 sub-session msgs, sub-session end marker
	require.Len(t, data.messages, 5)
	assert.Equal(t, "#1 [root]", data.messages[0].label)
	assert.True(t, data.messages[1].isSubSessionMarker(), "expected sub-session start marker")
	assert.Contains(t, data.messages[1].label, "sub-session start")
	assert.Equal(t, "#2 [sub-agent]", data.messages[2].label)
	assert.Equal(t, "#3 [sub-agent]", data.messages[3].label)
	assert.True(t, data.messages[4].isSubSessionMarker(), "expected sub-session end marker")
	assert.Contains(t, data.messages[4].label, "sub-session end")
	assert.Contains(t, data.messages[4].label, "$0.0030") // sub-session total cost
}

func TestCostDialogSubSessionRendersInView(t *testing.T) {
	t.Parallel()

	sess := session.New()

	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Creating sub-session",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 200,
			},
			Cost: 0.005,
		},
	})

	subSess := session.New()
	subSess.AddMessage(&session.Message{
		AgentName: "sub-agent",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Sub result",
			Model:   "gpt-4o-mini",
			Usage: &chat.Usage{
				InputTokens:  400,
				OutputTokens: 80,
			},
			Cost: 0.001,
		},
	})
	sess.AddSubSession(subSess)

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "sub-session start")
	assert.Contains(t, view, "sub-session end")
	assert.Contains(t, view, "sub-agent")
}

// TestGatherCostDataConcurrent pins the data-race fix for the cost dialog
// (#3591): gatherCostData used to walk the live sess.Messages slice
// (recursively for sub-sessions) and range MessageUsageHistory directly
// while runtime goroutines mutate them. It must iterate MessagesSnapshot()
// and MessageUsageHistorySnapshot() instead. Run with -race; restoring the
// direct reads makes the detector flag the concurrent AddMessage /
// AddMessageUsageRecord appends.
//
// The appended messages carry no usage, so every gatherCostData pass also
// takes the remote-mode fallback and reads the usage history concurrently
// with its writers. The post-Wait assertions only count the final records,
// so they are independent of goroutine scheduling.
func TestGatherCostDataConcurrent(t *testing.T) {
	t.Parallel()

	sub := session.New()
	sess := session.New()
	sess.AddSubSession(sub)
	d := &costDialog{session: sess}

	var wg sync.WaitGroup
	for i := range 50 {
		n := int64(i + 1)
		wg.Go(func() {
			sess.AddMessage(session.UserMessage("root"))
		})
		wg.Go(func() {
			sub.AddMessage(session.UserMessage("sub"))
		})
		wg.Go(func() {
			sess.AddMessageUsageRecord("agent", "model", 0.1, &chat.Usage{InputTokens: 10, OutputTokens: 5})
		})
		wg.Go(func() {
			sess.SetUsage(n, 2*n)
		})
		wg.Go(func() {
			sess.SetTokensAndCost(n, 2*n, float64(n))
		})
		wg.Go(func() {
			_ = d.gatherCostData()
		})
	}
	wg.Wait()

	data := d.gatherCostData()
	assert.Equal(t, 50, data.actualMessageCount())
	assert.InDelta(t, 5.0, data.total.cost, 0.0001)
	assert.Equal(t, int64(500), data.total.InputTokens)
	assert.Equal(t, int64(250), data.total.OutputTokens)
}

// TestGatherCostDataConcurrentSessionUsageFallback pins the session-level
// fallback of the same fix: with no per-message data at all, gatherCostData
// used to read d.session.InputTokens/OutputTokens directly, racing the
// runtime's SetUsage/SetTokensAndCost. It must take one Usage() snapshot,
// which also keeps the reported pair internally consistent: every writer
// stores an (n, 2n) pair, so any atomic snapshot satisfies the invariant
// regardless of scheduling. Run with -race.
func TestGatherCostDataConcurrentSessionUsageFallback(t *testing.T) {
	t.Parallel()

	sess := session.New()
	d := &costDialog{session: sess}

	var wg sync.WaitGroup
	for i := range 100 {
		n := int64(i + 1)
		wg.Go(func() {
			sess.SetUsage(n, 2*n)
		})
		wg.Go(func() {
			sess.SetTokensAndCost(n, 2*n, float64(n))
		})
		wg.Go(func() {
			data := d.gatherCostData()
			if data.total.OutputTokens != 2*data.total.InputTokens {
				t.Errorf("torn usage pair: input=%d output=%d", data.total.InputTokens, data.total.OutputTokens)
			}
		})
	}
	wg.Wait()
}

func TestFormatCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cost     float64
		expected string
	}{
		{0.0, "$0.00"},
		{0.00001, "$0.00"},
		{0.0001, "$0.0001"},
		{0.001, "$0.0010"},
		{0.01, "$0.01"},
		{0.1, "$0.10"},
		{1.0, "$1.00"},
		{10.5, "$10.50"},
	}

	for _, tt := range tests {
		result := formatCost(tt.cost)
		assert.Equal(t, tt.expected, result, "formatCost(%f)", tt.cost)
	}
}

func TestFormatCostNegative(t *testing.T) {
	t.Parallel()

	// Negative costs should format with a leading "-" prefix.
	assert.Equal(t, "-$0.01", formatCost(-0.01))
	assert.Equal(t, "-$0.0050", formatCost(-0.005))
	assert.Equal(t, "-$1.00", formatCost(-1.0))
	// Very small negative is clamped to zero.
	assert.Equal(t, "-$0.00", formatCost(-0.00001))
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		count    int64
		expected string
	}{
		{0, "0"},
		{100, "100"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{10000, "10.0K"},
		{999999, "1000.0K"},
		{1000000, "1.0M"},
		{1500000, "1.5M"},
		{10000000, "10.0M"},
	}

	for _, tt := range tests {
		result := formatTokenCount(tt.count)
		assert.Equal(t, tt.expected, result, "formatTokenCount(%d)", tt.count)
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d        time.Duration
		expected string
	}{
		{-5 * time.Second, "0s"},  // negative durations clamp to 0
		{-90 * time.Second, "0s"}, // negative durations clamp to 0
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m 30s"},
		{5 * time.Minute, "5m"},
		{60 * time.Minute, "1h"},
		{90 * time.Minute, "1h 30m"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}

	for _, tt := range tests {
		result := formatDuration(tt.d)
		assert.Equal(t, tt.expected, result, "formatDuration(%v)", tt.d)
	}
}

func TestCostDialogRefreshesOnLiveSessionUpdates(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage:   &chat.Usage{InputTokens: 1000, OutputTokens: 500},
			Cost:    0.005,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()
	assert.Contains(t, view, "#1")
	assert.NotContains(t, view, "#2")

	// A message streamed in while the dialog is open must show up.
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "World",
			Model:   "gpt-4o",
			Usage:   &chat.Usage{InputTokens: 800, OutputTokens: 300},
			Cost:    0.003,
		},
	})

	view = dialog.View()
	assert.Contains(t, view, "#2")
}

func TestCostDialogUsageOnlyResponses(t *testing.T) {
	t.Parallel()

	sess := session.New()
	usage := chat.Usage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 50}
	sess.AddMessage(&session.Message{
		AgentName: "worker",
		Message: chat.Message{
			Role: chat.MessageRoleAssistant, ReasoningContent: "thinking",
			Model: "test/model", Cost: 0.004, Usage: &usage,
		},
	})

	data := (&costDialog{session: sess}).gatherCostData()
	assert.InDelta(t, 0.004, data.total.cost, 1e-9)
	assert.Equal(t, usage, data.total.Usage)
	require.Len(t, data.agents, 1)
	assert.Equal(t, "worker", data.agents[0].label)
	assert.InDelta(t, 0.004, data.agents[0].cost, 1e-9)
	require.Len(t, data.models, 1)
	assert.Equal(t, "test/model", data.models[0].label)
	assert.InDelta(t, 0.004, data.models[0].cost, 1e-9)
	require.Len(t, data.messages, 1)
	assert.InDelta(t, 0.004, data.messages[0].cost, 1e-9)
}
