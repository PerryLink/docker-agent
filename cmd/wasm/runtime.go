//go:build js && wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/embeddedchat"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// sessionOptions are the host-supplied inputs for one chat session.
type sessionOptions struct {
	YAML      string
	AgentName string
	// Env is the only environment the session sees: API keys, MCP header
	// values, ${env.X} placeholders. Nothing is copied into the process env.
	Env map[string]string
	// ToolProxy is the trusted HTTPS egress proxy SSRF-guarded requests
	// (remote MCP) are routed through; without it they fail closed.
	ToolProxy        string
	OAuthRedirectURI string
	// AutoApprove runs tool calls without asking for confirmation, like
	// `--yolo`; otherwise each call the safety policy does not clear is
	// surfaced as a tool_confirmation event.
	AutoApprove bool
	// History seeds the first conversation (the stateless chat API).
	History []chat.Message
}

// host holds the registries every session is built from. The browser entry
// point uses the demo providers and the browser toolsets; tests inject
// mocked ones.
type host struct {
	providers *provider.Registry
	toolsets  teamloader.ToolsetRegistry
}

var browserHost = host{providers: demoProviders, toolsets: browserToolsets}

// browserToolsets is the only toolset registry the browser build exposes:
// remote MCP servers, reached over HTTP. Everything else needs a process,
// a filesystem or a socket the browser does not have.
var browserToolsets = teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{"mcp": mcpCreator})

func mcpCreator(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	if err := checkRemoteMCP(toolset); err != nil {
		return nil, err
	}
	return mcptools.Creator(ctx, toolset, parentDir, runConfig, configName)
}

// browserBuiltinHooks are the builtin hooks that neither run a process nor
// touch the filesystem or git.
var browserBuiltinHooks = []string{
	builtins.AddContext,
	builtins.AddDate,
	builtins.AddEnvironmentInfo,
	builtins.LimitLargeToolResults,
	builtins.MaxIterations,
	builtins.RedactSecrets,
}

// modelsStore is the models.dev catalog baked into the binary: the browser
// has no cache directory to refresh a live copy into.
var modelsStore = sync.OnceValue(func() *modelsdev.Store {
	return modelsdev.NewDatabaseStore(modelsdev.EmbeddedSnapshot())
})

// lifetimeContext returns the context a session lives in. It carries the
// egress proxy so every request the runtime makes on the session's behalf,
// including MCP connections started lazily during a turn, is routed
// through it.
func lifetimeContext(parent context.Context, toolProxy string) (context.Context, context.CancelFunc, error) {
	ctx := mcptools.WithOAuthTokenStore(parent, mcptools.NewInMemoryTokenStore())
	if toolProxy != "" {
		var err error
		if ctx, err = httpclient.WithEgressProxy(ctx, toolProxy); err != nil {
			return nil, nil, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	return ctx, cancel, nil
}

// newSession loads the YAML with the browser registries and starts an
// embedded chat session for it. The config is audited first so anything
// needing a host process or local files is rejected up front rather than
// skipped with a warning at load time.
func (h host) newSession(ctx context.Context, opts sessionOptions) (*embeddedchat.Session, error) {
	source := config.NewBytesSource("config.yaml", []byte(opts.YAML))
	cfg, err := config.Load(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := checkBrowserConfig(cfg, opts.AgentName); err != nil {
		return nil, err
	}

	runConfig := &config.RuntimeConfig{
		Config:                 config.Config{WorkingDir: "/"},
		EnvProviderOverride:    environment.NewMapEnvProvider(opts.Env),
		ModelsDevStoreOverride: modelsStore(),
	}

	runtimeOpts := []runtime.Opt{
		runtime.WithWorkingDir("/"),
		runtime.WithModelStore(modelsStore()),
		runtime.WithProviderRegistry(h.providers),
		// The host owns the browser: it opens the authorize URL and relays
		// the callback through respondToElicitation.
		runtime.WithManagedOAuth(false),
		runtime.WithUnmanagedOAuthRedirectURI(opts.OAuthRedirectURI),
	}
	if opts.AgentName != "" {
		runtimeOpts = append(runtimeOpts, runtime.WithCurrentAgent(opts.AgentName))
	}

	var sessionOpts []session.Opt
	if opts.AutoApprove {
		sessionOpts = append(sessionOpts, session.WithToolsApproved(true))
	}
	var initial *session.Session
	if len(opts.History) > 0 {
		items := make([]session.Item, 0, len(opts.History))
		for i := range opts.History {
			items = append(items, session.NewMessageItem(&session.Message{Message: opts.History[i]}))
		}
		initial = session.New(append(slices.Clone(sessionOpts), session.WithMessages(items))...)
	}

	return embeddedchat.New(ctx, embeddedchat.Config{
		AgentSource:   source,
		RuntimeConfig: runConfig,
		LoadOpts: []teamloader.Opt{
			teamloader.WithProviderRegistry(h.providers),
			teamloader.WithToolsetRegistry(h.toolsets),
			teamloader.WithStrict(config.FeatureHooks),
		},
		RuntimeOptions:     runtimeOpts,
		SessionOptions:     sessionOpts,
		InitialSession:     initial,
		NonLocal:           true,
		ForwardElicitation: true,
		ForwardAllEvents:   true,
	})
}

// checkBrowserConfig rejects config that strict loading would accept but
// that cannot work in the browser: local files, host processes, and MCP
// servers that are not plain remote endpoints.
func checkBrowserConfig(cfg *latest.Config, agentName string) error {
	if agentName != "" && !slices.ContainsFunc(cfg.Agents, func(a latest.AgentConfig) bool { return a.Name == agentName }) {
		return fmt.Errorf("agent %q not found", agentName)
	}
	var errs []error
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		loc := "agents." + a.Name
		if len(a.AddPromptFiles) > 0 {
			errs = append(errs, fmt.Errorf("%s.add_prompt_files: local files are not available in the browser", loc))
		}
		if a.Cache != nil && a.Cache.Enabled && a.Cache.Path != "" {
			errs = append(errs, fmt.Errorf("%s.cache.path: local files are not available in the browser", loc))
		}
		for j, ts := range a.Toolsets {
			if ts.Type == "mcp" {
				if err := checkRemoteMCP(ts); err != nil {
					errs = append(errs, fmt.Errorf("%s.toolsets[%d]: %w", loc, j, err))
				}
			}
		}
		errs = append(errs, checkHooks(a.Hooks, loc+".hooks")...)
	}
	return errors.Join(errs...)
}

// checkRemoteMCP accepts only MCP toolsets that connect to a remote server:
// stdio servers and catalog references need a host process, and the
// local-only fields would otherwise be ignored silently.
func checkRemoteMCP(ts latest.Toolset) error {
	switch {
	case ts.Command != "" || len(ts.Args) > 0:
		return errors.New("stdio MCP servers need a host process; only remote servers are supported in the browser")
	case ts.Ref != "":
		return errors.New("MCP catalog references need the Docker MCP gateway; only remote servers are supported in the browser")
	case ts.Remote.URL == "":
		return errors.New("mcp toolset requires remote.url in the browser")
	case ts.WorkingDir != "" || len(ts.Env) > 0 || ts.Config != nil || ts.Version != "" || ts.Path != "":
		return errors.New("working_dir, env, config, version and path only apply to local MCP servers")
	}
	return nil
}

// checkHooks accepts only builtin hooks from browserBuiltinHooks: command
// hooks spawn a process and the other builtins read files or run git.
func checkHooks(cfg *latest.HooksConfig, loc string) []error {
	var errs []error
	for event, matchers := range cfg.Events() {
		for _, matcher := range matchers {
			for i, hook := range matcher.Hooks {
				switch {
				case hook.Type != "builtin":
					errs = append(errs, fmt.Errorf("%s.%s[%d]: %s hooks are not supported in the browser; use builtin hooks", loc, event, i, hook.Type))
				case !slices.Contains(browserBuiltinHooks, hook.Command):
					errs = append(errs, fmt.Errorf("%s.%s[%d]: builtin %q is not available in the browser (supported: %v)", loc, event, i, hook.Command, browserBuiltinHooks))
				}
			}
		}
	}
	return errs
}
