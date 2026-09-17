package vertex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestNewClient_RejectsFullThinkingDisplayOnUnsupportedModel(t *testing.T) {
	t.Parallel()
	cfg := &latest.ModelConfig{
		Provider:     "anthropic",
		Model:        "claude-sonnet-5",
		ProviderOpts: map[string]any{"thinking_display": "display"},
	}
	// Validation runs before GCP credential discovery, so no credentials needed.
	_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil), "project", "location")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support thinking_display")
}

func TestNewClient_RequiresProjectAndLocation(t *testing.T) {
	t.Parallel()
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	env := environment.NewMapEnvProvider(nil)

	_, err := NewClient(t.Context(), cfg, env, "", "location")
	require.ErrorContains(t, err, "requires a GCP project")

	_, err = NewClient(t.Context(), cfg, env, "project", "")
	require.ErrorContains(t, err, "requires a GCP location")
}

func TestNewClient_RequiresConfigAndEnv(t *testing.T) {
	t.Parallel()

	_, err := NewClient(t.Context(), nil, environment.NewMapEnvProvider(nil), "project", "location")
	require.ErrorContains(t, err, "model configuration is required")

	_, err = NewClient(t.Context(), &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}, nil, "project", "location")
	require.ErrorContains(t, err, "environment provider is required")
}

func TestRejectClaudeAPIOnlyFeatures(t *testing.T) {
	t.Parallel()
	for _, feature := range []string{"native_compaction", "cache_diagnostics"} {
		t.Run(feature, func(t *testing.T) {
			cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5", ProviderOpts: map[string]any{feature: true}}
			_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(nil), "project", "location")
			require.ErrorContains(t, err, "requires the Claude API")
		})
	}
}
