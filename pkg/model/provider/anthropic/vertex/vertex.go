// Package vertex provides an Anthropic client for Claude models hosted on
// Google Cloud's Vertex AI. It lives in its own package so that importing
// the core anthropic provider does not pull the Google Cloud auth stack
// (cloud.google.com/go/auth, google.golang.org/api, grpc, ...).
package vertex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	sdkvertex "github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// cloudPlatformScope is the OAuth2 scope required for Vertex AI API access.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// NewClient creates a new Anthropic client that talks to Claude models
// hosted on Google Cloud's Vertex AI via the Anthropic-native endpoints
// (`:rawPredict` and `:streamRawPredict`), authenticated with Google
// Application Default Credentials.
//
// This is required because Anthropic models on Vertex AI do not support the
// OpenAI-compatible `/chat/completions` endpoint and fail with
// `FAILED_PRECONDITION: The deployed model does not support ChatCompletions.`
//
// See: https://docs.anthropic.com/en/api/claude-on-vertex-ai
func NewClient(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, project, location string, opts ...options.Opt) (*anthropic.Client, error) {
	if cfg == nil {
		return nil, errors.New("model configuration is required")
	}
	for _, name := range []string{"native_compaction", "cache_diagnostics"} {
		if enabled, _ := providerutil.GetProviderOptBool(cfg.ProviderOpts, name); enabled {
			return nil, fmt.Errorf("%s requires the Claude API and is not available on Vertex AI", name)
		}
	}
	if env == nil {
		return nil, errors.New("environment provider is required")
	}
	if project == "" {
		return nil, errors.New("vertex AI requires a GCP project")
	}
	if location == "" {
		return nil, errors.New("vertex AI requires a GCP location")
	}

	// Config validation (thinking_display, ...) runs inside
	// NewClientFromFactory before the factory below, so configuration errors
	// surface without requiring GCP credentials.
	anthropicClient, err := anthropic.NewClientFromFactory(ctx, cfg, env, func(ctx context.Context) (anthropicsdk.Client, error) {
		// Resolve GCP credentials up front so we can return a descriptive error
		// instead of the panic that vertex.WithGoogleAuth would raise.
		creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
		if err != nil {
			return anthropicsdk.Client{}, fmt.Errorf("failed to obtain GCP credentials for Vertex AI: %w (run 'gcloud auth application-default login')", err)
		}

		slog.DebugContext(ctx, "Creating Anthropic client for Vertex AI",
			"project", project,
			"location", location,
			"model", cfg.Model,
		)

		// vertex.WithCredentials configures the base URL, Google-authenticated
		// HTTP client, and middleware that rewrites /v1/messages requests to the
		// Anthropic-native Vertex AI endpoints (`:rawPredict` / `:streamRawPredict`)
		// and injects the `anthropic_version: vertex-2023-10-16` body field.
		//
		// The explicit option.WithAPIKey("") is REQUIRED (do not remove): the
		// anthropic SDK's NewClient applies DefaultClientOptions() first, which
		// auto-reads ANTHROPIC_API_KEY from the environment and sets the
		// X-Api-Key header. On Vertex AI the request is authenticated with
		// OAuth2 (via the google transport in vertex.WithCredentials), so we
		// must clear the stray X-Api-Key header that would otherwise leak a
		// direct-API credential into Google's infrastructure.
		return anthropicsdk.NewClient(
			sdkvertex.WithCredentials(ctx, location, project, creds),
			option.WithAPIKey(""),
		), nil
	}, opts...)
	if err != nil {
		return nil, err
	}

	slog.DebugContext(ctx, "Anthropic (Vertex AI) client created successfully", "model", cfg.Model)
	return anthropicClient, nil
}
