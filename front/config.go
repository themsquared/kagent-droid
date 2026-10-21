package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// kagent compiles the AgentTemplate into KAGENT_CONFIG_JSON (also written to
// /config/config.json). kagent Enterprise 1.0.0-alpha4 sends the agent config
// flat; newer kagent wraps it as {"runtime": {...}, "agent": {...}}.
type mcpParams struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

type mcpTool struct {
	Params          mcpParams `json:"params"`
	RequireApproval bool      `json:"require_approval"`
}

type agentConfig struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Instruction string `json:"instruction"`
	Model       struct {
		Model   string `json:"model"`
		BaseURL string `json:"base_url"`
		OpenAI  struct {
			BaseURL string `json:"base_url"`
		} `json:"openai"`
	} `json:"model"`
	HTTPTools []mcpTool `json:"http_tools"`
	SSETools  []mcpTool `json:"sse_tools"`
}

type runtimeConfig struct {
	Name string `json:"name"`
	Port uint32 `json:"port"`
}

type harnessConfig struct {
	Agent     agentConfig
	Runtime   runtimeConfig
	Model     string
	Autonomy  string
	StateFile string
	Cwd       string
	MCPNames  []string
	AgentCard []byte
}

func readInput(env, path string) []byte {
	if v := os.Getenv(env); v != "" {
		return []byte(v)
	}
	b, _ := os.ReadFile(path)
	return b
}

func loadConfig(log *slog.Logger) (*harnessConfig, error) {
	// Substrate gives every Actor a DurableDir at /data. Keep everything that
	// must outlive a turn there: droid's HOME (its session store and mcp.json),
	// the workspace, and the context -> droid session map.
	state := envOr("DROID_STATE_DIR", "/data")
	if fi, err := os.Stat(state); err == nil && fi.IsDir() {
		home := filepath.Join(state, "home")
		if err := os.MkdirAll(home, 0o700); err == nil {
			os.Setenv("HOME", home)
		}
	} else {
		state = os.TempDir()
	}
	cfg := &harnessConfig{
		Autonomy:  envOr("DROID_AUTONOMY", "high"),
		Cwd:       envOr("DROID_CWD", filepath.Join(state, "workspace")),
		StateFile: filepath.Join(state, "droid-sessions.json"),
		AgentCard: readInput("KAGENT_AGENT_CARD_JSON", "/config/agent-card.json"),
	}
	raw := readInput("KAGENT_CONFIG_JSON", "/config/config.json")
	if len(raw) > 0 {
		var wrapped struct {
			Runtime *runtimeConfig   `json:"runtime"`
			Agent   *json.RawMessage `json:"agent"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return nil, fmt.Errorf("parse kagent config: %w", err)
		}
		agentRaw := raw
		if wrapped.Agent != nil {
			agentRaw = *wrapped.Agent
		}
		if wrapped.Runtime != nil {
			cfg.Runtime = *wrapped.Runtime
		}
		if err := json.Unmarshal(agentRaw, &cfg.Agent); err != nil {
			return nil, fmt.Errorf("parse kagent agent config: %w", err)
		}
	}

	cfg.Model = resolveModel(envOr("DROID_MODEL", cfg.Agent.Model.Model))

	// Factory auth rides Substrate credential injection. An OpenAI-type
	// ModelConfig with baseUrl https://api.factory.ai makes the egress gateway
	// allow that origin and swap the real key into "Authorization: Bearer".
	// kagent gives the Actor an inert placeholder in OPENAI_API_KEY; droid
	// sends it as FACTORY_API_KEY and the gateway replaces it in flight.
	base := cfg.Agent.Model.BaseURL
	if base == "" {
		base = cfg.Agent.Model.OpenAI.BaseURL
	}
	if strings.Contains(base, "factory.ai") && os.Getenv("FACTORY_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") != "" {
		os.Setenv("FACTORY_API_KEY", os.Getenv("OPENAI_API_KEY"))
		log.Info("using gateway-injected Factory credential", "origin", base)
	}

	if os.Getenv("DROID_HARNESS_DEBUG") != "" {
		// Gateway placeholders are inert by design; log only their leading characters.
		log.Info("debug placeholder shape", "OPENAI_API_KEY", prefix(os.Getenv("OPENAI_API_KEY"), 40))
		for _, t := range append(cfg.Agent.HTTPTools, cfg.Agent.SSETools...) {
			for k, v := range t.Params.Headers {
				log.Info("debug placeholder shape", "url", t.Params.URL, "header", k, "value", prefix(v, 40))
			}
		}
	}
	names, err := writeMCPConfig(cfg.Agent, log)
	if err != nil {
		return nil, err
	}
	cfg.MCPNames = names
	return cfg, nil
}

// Factory model IDs carry a date suffix that kagent ModelConfig IDs do not.
var modelAliases = map[string]string{
	"claude-haiku-4-5":  "claude-haiku-4-5-20251001",
	"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
	"claude-opus-4-5":   "claude-opus-4-5-20251101",
}

func resolveModel(m string) string {
	if v, ok := modelAliases[m]; ok {
		return v
	}
	return m
}

var nonName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func serverName(raw string, i int) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Sprintf("mcp-%d", i)
	}
	// In-cluster URLs are http://<svc>.<ns>.svc...: the Service name is the useful part.
	return nonName.ReplaceAllString(strings.SplitN(u.Hostname(), ".", 2)[0], "-")
}

// writeMCPConfig turns the AgentTemplate's MCP servers into ~/.factory/mcp.json.
func writeMCPConfig(a agentConfig, log *slog.Logger) ([]string, error) {
	servers := map[string]any{}
	var names []string
	add := func(tools []mcpTool, kind string, offset int) {
		for i, t := range tools {
			name := serverName(t.Params.URL, offset+i)
			if isFactoryOrigin(t.Params.URL) {
				// Egress-only entry (deploy/factory-egress.yaml): it exists so
				// Substrate allows droid's own traffic to that host.
				log.Info("factory origin allowed for egress, not used as MCP", "url", t.Params.URL)
				continue
			}
			if t.RequireApproval {
				// No human-in-the-loop path through A2A yet, so a server that
				// needs approval is left out rather than auto-approved.
				log.Warn("skipping MCP server that requires approval", "server", name)
				continue
			}
			entry := map[string]any{"type": kind, "url": t.Params.URL, "disabled": false}
			if len(t.Params.Headers) > 0 {
				entry["headers"] = t.Params.Headers
			}
			servers[name] = entry
			names = append(names, name)
		}
	}
	add(a.HTTPTools, "http", 0)
	add(a.SSETools, "sse", len(a.HTTPTools))
	if len(servers) == 0 {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return names, os.WriteFile(filepath.Join(dir, "mcp.json"), b, 0o600)
}

func isFactoryOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Hostname() == "factory.ai" || strings.HasSuffix(u.Hostname(), ".factory.ai"))
}

func prefix(s string, n int) string {
	if len(s) > n {
		return s[:n] + fmt.Sprintf("…(len %d)", len(s))
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
