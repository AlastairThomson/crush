package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/crush/internal/home"
	"gopkg.in/yaml.v3"
)

// Custom sub-agents are Claude-Code-style Markdown files with YAML
// frontmatter (name, description, tools, model) plus a system-prompt body.
// They are discovered from disk and merged into the agent map by
// SetupAgents. See internal/skills for the sibling skills loader this
// mirrors.

const (
	maxAgentNameLength        = 64
	maxAgentDescriptionLength = 1024

	// Sub-agent-spawning tools are always stripped from custom agents:
	// allowing them would let buildTools -> agentTool -> buildAgent
	// re-enter unboundedly (see agent_tool.go), and Claude Code sub-agents
	// cannot themselves spawn sub-agents.
	agentToolName        = "agent"
	agenticFetchToolName = "agentic_fetch"
)

var agentNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

// claudeToolAliases maps Claude Code tool names to their Crush equivalents.
// Native Crush tool names (lower-case) are accepted directly and do not need
// an entry here.
var claudeToolAliases = map[string]string{
	"read":      "view",
	"write":     "write",
	"edit":      "edit",
	"multiedit": "multiedit",
	"grep":      "grep",
	"glob":      "glob",
	"bash":      "bash",
	"ls":        "ls",
	"webfetch":  "fetch",
	"todowrite": "todos",
	"task":      agentToolName, // Claude's Task tool == sub-agent; stripped below.
}

// flexStringList accepts either a YAML sequence (["a", "b"]) or a scalar
// string ("a, b") and normalizes to a trimmed, non-empty []string. A nil
// value means the key was absent (distinct from an explicit empty list).
type flexStringList []string

func (f *flexStringList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		*f = splitCommaList(value.Value)
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := value.Decode(&items); err != nil {
			return err
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			if it = strings.TrimSpace(it); it != "" {
				out = append(out, it)
			}
		}
		*f = out
		return nil
	default:
		return fmt.Errorf("expected a string or list, got yaml kind %d", value.Kind)
	}
}

func splitCommaList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

type customAgentFrontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Tools       flexStringList `yaml:"tools"`
	Model       string         `yaml:"model"`
	Skills      flexStringList `yaml:"skills"`
}

// GlobalAgentsDirs returns the user-global directories scanned for custom
// sub-agent definitions. It mirrors GlobalSkillsDirs and intentionally scans
// both the Crush-native location and Claude Code's ~/.claude/agents so
// existing Claude Code agent files work unchanged.
func GlobalAgentsDirs() []string {
	if dir := os.Getenv("CRUSH_AGENTS_DIR"); dir != "" {
		return []string{dir}
	}

	paths := []string{
		filepath.Join(home.Config(), appName, "agents"),
		filepath.Join(home.Dir(), ".claude", "agents"),
	}

	if runtime.GOOS == "windows" {
		appData := cmp.Or(
			os.Getenv("LOCALAPPDATA"),
			filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local"),
		)
		paths = append(paths, filepath.Join(appData, appName, "agents"))
	}

	return paths
}

// projectAgentSubdirs lists the conventional project-level directories where
// custom sub-agents are discovered. Shared across working-dir and git-root
// lookups to prevent drift.
var projectAgentSubdirs = []string{
	".crush/agents",
	".claude/agents",
	".cursor/agents",
}

// ProjectAgentsDir returns the project directories scanned for custom
// sub-agents. Like ProjectSkillsDir it also checks the git working-tree root
// so monorepo-level agents are found from a subdirectory. Working-directory
// paths come first so local agents take precedence.
func ProjectAgentsDir(workingDir string) []string {
	dirs := make([]string, 0, len(projectAgentSubdirs)*2)
	for _, sub := range projectAgentSubdirs {
		dirs = append(dirs, filepath.Join(workingDir, sub))
	}
	if root := worktreeRoot(workingDir); root != "" && root != workingDir {
		for _, sub := range projectAgentSubdirs {
			dirs = append(dirs, filepath.Join(root, sub))
		}
	}
	return dirs
}

// toCrushTools maps a raw frontmatter tool list (Claude or native names) to
// validated Crush tool names, dropping unknowns and always stripping the
// sub-agent-spawning tools.
func toCrushTools(raw []string) []string {
	valid := make(map[string]bool)
	for _, n := range allToolNames() {
		valid[n] = true
	}

	seen := make(map[string]bool)
	var out []string
	for _, t := range raw {
		key := strings.ToLower(strings.TrimSpace(t))
		if key == "" {
			continue
		}
		name, ok := claudeToolAliases[key]
		if !ok {
			if !valid[key] {
				slog.Warn("Unknown tool in custom agent; ignoring", "tool", t)
				continue
			}
			name = key
		}
		if name == agentToolName || name == agenticFetchToolName {
			continue
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// stripSubAgentTools removes the sub-agent-spawning tools from an inherited
// tool set.
func stripSubAgentTools(tools []string) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t == agentToolName || t == agenticFetchToolName {
			continue
		}
		out = append(out, t)
	}
	return out
}

// toModelType maps a frontmatter model value onto Crush's two model slots.
// Crush resolves only "large" and "small" globally; Claude aliases and any
// unknown value fall back to large with a warning.
func toModelType(raw string) SelectedModelType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "large":
		return SelectedModelTypeLarge
	case "small":
		return SelectedModelTypeSmall
	default:
		slog.Warn("Unsupported model in custom agent; defaulting to large", "model", raw)
		return SelectedModelTypeLarge
	}
}

func validateAgentName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > maxAgentNameLength {
		return fmt.Errorf("name exceeds %d characters", maxAgentNameLength)
	}
	if !agentNamePattern.MatchString(name) {
		return errors.New("name must be alphanumeric with hyphens, no leading/trailing/consecutive hyphens")
	}
	return nil
}

// parseCustomAgent reads and validates a single custom-agent Markdown file.
// inheritTools is the tool set a custom agent inherits when its frontmatter
// omits `tools` entirely.
func parseCustomAgent(path string, inheritTools []string) (*Agent, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	frontmatter, body, err := splitAgentFrontmatter(string(content))
	if err != nil {
		return nil, err
	}

	var fm customAgentFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &fm); err != nil {
		return nil, fmt.Errorf("parsing frontmatter: %w", err)
	}

	name := strings.TrimSpace(fm.Name)
	if err := validateAgentName(name); err != nil {
		return nil, err
	}
	desc := strings.TrimSpace(fm.Description)
	if desc == "" {
		return nil, errors.New("description is required")
	}
	if len(desc) > maxAgentDescriptionLength {
		return nil, fmt.Errorf("description exceeds %d characters", maxAgentDescriptionLength)
	}

	// tools absent (nil) -> inherit; present (even empty) -> exactly that set.
	var allowed []string
	if fm.Tools == nil {
		allowed = stripSubAgentTools(inheritTools)
	} else {
		allowed = toCrushTools(fm.Tools)
	}

	return &Agent{
		ID:           name,
		Name:         name,
		Description:  desc,
		Model:        toModelType(fm.Model),
		AllowedTools: allowed,
		// Custom agents get no MCP tools by default, matching the built-in
		// task sub-agent's tight context.
		AllowedMCP:   map[string][]string{},
		Skills:       []string(fm.Skills),
		SystemPrompt: strings.TrimSpace(body),
		Source:       path,
	}, nil
}

// discoverCustomAgents walks the given directories for *.md custom-agent files
// and returns the parsed agents. Parse/validation failures are logged and
// skipped (non-fatal). Reserved ids (coder/task) are skipped. On name
// collision the last-seen definition wins, so directories should be passed in
// ascending precedence order (global before project).
func discoverCustomAgents(paths []string, inheritTools []string) []Agent {
	var agents []Agent
	seenPath := make(map[string]bool)
	byName := make(map[string]int)

	for _, dir := range paths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Debug("Failed to read agents directory", "dir", dir, "error", err)
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if seenPath[path] {
				continue
			}
			seenPath[path] = true

			agent, err := parseCustomAgent(path, inheritTools)
			if err != nil {
				slog.Warn("Failed to load custom agent", "path", path, "error", err)
				continue
			}
			if agent.ID == AgentCoder || agent.ID == AgentTask {
				slog.Warn("Custom agent uses a reserved name; skipping", "name", agent.ID, "path", path)
				continue
			}
			if idx, ok := byName[agent.ID]; ok {
				agents[idx] = *agent // last wins
			} else {
				byName[agent.ID] = len(agents)
				agents = append(agents, *agent)
			}
		}
	}

	slices.SortFunc(agents, func(a, b Agent) int {
		return strings.Compare(a.ID, b.ID)
	})
	return agents
}

// splitAgentFrontmatter extracts YAML frontmatter and body from markdown
// content. Copied from the skills loader to keep config decoupled from the
// skills package.
func splitAgentFrontmatter(content string) (frontmatter, body string, err error) {
	// Strip UTF-8 BOM for compatibility with editors that include it.
	content = strings.TrimPrefix(content, "\uFEFF")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	start := slices.IndexFunc(lines, func(line string) bool {
		return strings.TrimSpace(line) != ""
	})
	if start == -1 || strings.TrimSpace(lines[start]) != "---" {
		return "", "", errors.New("no YAML frontmatter found")
	}

	endOffset := slices.IndexFunc(lines[start+1:], func(line string) bool {
		return strings.TrimSpace(line) == "---"
	})
	if endOffset == -1 {
		return "", "", errors.New("unclosed frontmatter")
	}
	end := start + 1 + endOffset

	frontmatter = strings.Join(lines[start+1:end], "\n")
	body = strings.Join(lines[end+1:], "\n")
	return frontmatter, body, nil
}
