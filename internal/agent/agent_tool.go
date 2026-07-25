package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt       string `json:"prompt" description:"The task for the agent to perform"`
	SubagentType string `json:"subagent_type,omitempty" description:"Which sub-agent to delegate to (see the list in this tool's description). Omit to use the default search agent."`
}

const (
	AgentToolName = "agent"
)

// subAgentEntry is a built sub-agent plus the description shown to the
// delegating agent.
type subAgentEntry struct {
	agent       SessionAgent
	description string
}

func (c *coordinator) agentTool(ctx context.Context) (fantasy.AgentTool, error) {
	cfg := c.cfg.Config()

	taskCfg, ok := cfg.Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}

	// The default sub-agent (empty subagent_type) is the built-in task agent.
	taskP, err := taskPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}
	taskAgent, err := c.buildAgent(ctx, taskP, taskCfg, true)
	if err != nil {
		return nil, err
	}

	// Build every custom sub-agent (everything that isn't coder or task).
	subAgents := map[string]subAgentEntry{}
	for id, agentCfg := range cfg.Agents {
		if id == config.AgentCoder || id == config.AgentTask || agentCfg.Disabled {
			continue
		}
		// Defensive recursion guard: a sub-agent must never be able to spawn
		// sub-agents. config strips the agent tool at load; refuse anything
		// that slipped through rather than risk unbounded re-entry.
		if slices.Contains(agentCfg.AllowedTools, AgentToolName) {
			slog.Warn("Custom agent requests the agent tool; skipping to avoid recursion", "agent", id)
			continue
		}

		opts := []prompt.Option{prompt.WithWorkingDir(c.cfg.WorkingDir())}
		if xml := c.resolveAgentSkills(agentCfg); xml != "" {
			opts = append(opts, prompt.WithLoadedSkills(xml))
		}
		p, err := subagentPrompt(agentCfg.SystemPrompt, opts...)
		if err != nil {
			slog.Warn("Failed to build custom agent prompt", "agent", id, "error", err)
			continue
		}
		built, err := c.buildAgent(ctx, p, agentCfg, true)
		if err != nil {
			slog.Warn("Failed to build custom agent", "agent", id, "error", err)
			continue
		}
		subAgents[id] = subAgentEntry{agent: built, description: agentCfg.Description}
	}

	return fantasy.NewParallelAgentTool(
		AgentToolName,
		buildAgentToolDescription(subAgents),
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}

			// Resolve which sub-agent handles this call.
			selected := taskAgent
			sessionTitle := "New Agent Session"
			if st := strings.TrimSpace(params.SubagentType); st != "" {
				entry, ok := subAgents[st]
				if !ok {
					return fantasy.NewTextErrorResponse(unknownSubagentMessage(st, subAgents)), nil
				}
				selected = entry.agent
				sessionTitle = st + " agent"
			}

			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}

			agentMessageID := tools.GetMessageFromContext(ctx)
			if agentMessageID == "" {
				return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
			}

			// The selected sub-agent's tools and system prompt are populated
			// asynchronously by buildAgent. Wait for that to finish before
			// running it so it never executes with an empty tool set/prompt.
			if err := c.readyWg.Wait(); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("agent not ready: %s", err)), nil
			}

			return c.runSubAgent(ctx, subAgentParams{
				Agent:          selected,
				SessionID:      sessionID,
				AgentMessageID: agentMessageID,
				ToolCallID:     call.ID,
				Prompt:         params.Prompt,
				SessionTitle:   sessionTitle,
			})
		},
	), nil
}

// resolveAgentSkills resolves a custom agent's declared skill names against the
// discovered skills and returns their concatenated <loaded_skill> XML for
// injection into the agent's system prompt. Unknown names are warned + skipped.
func (c *coordinator) resolveAgentSkills(agentCfg config.Agent) string {
	if len(agentCfg.Skills) == 0 {
		return ""
	}
	byName := make(map[string]*skills.Skill, len(c.allSkills))
	for _, s := range c.allSkills {
		byName[s.Name] = s
	}
	var sb strings.Builder
	for _, name := range agentCfg.Skills {
		s, ok := byName[name]
		if !ok {
			slog.Warn("Custom agent declares an unknown skill; skipping", "agent", agentCfg.ID, "skill", name)
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(s.FormatInvocation())
	}
	return sb.String()
}

// buildAgentToolDescription appends the catalog of available sub-agents to the
// base agent-tool description so the delegating agent can choose a
// subagent_type. fantasy generates the tool schema by reflection and cannot
// take a dynamic enum, so the names live in the description prose.
func buildAgentToolDescription(subAgents map[string]subAgentEntry) string {
	if len(subAgents) == 0 {
		return agentToolDescription
	}
	names := make([]string, 0, len(subAgents))
	for name := range subAgents {
		names = append(names, name)
	}
	slices.Sort(names)

	var sb strings.Builder
	sb.WriteString(agentToolDescription)
	sb.WriteString("\n\nSet `subagent_type` to one of the following to delegate to a specialized agent (omit it to use the default search agent):\n")
	for _, name := range names {
		fmt.Fprintf(&sb, "- %s: %s\n", name, subAgents[name].description)
	}
	return sb.String()
}

func unknownSubagentMessage(requested string, subAgents map[string]subAgentEntry) string {
	names := make([]string, 0, len(subAgents))
	for name := range subAgents {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return fmt.Sprintf("unknown subagent_type %q; no custom sub-agents are configured (omit subagent_type to use the default search agent)", requested)
	}
	return fmt.Sprintf("unknown subagent_type %q; available: %s (or omit subagent_type for the default search agent)", requested, strings.Join(names, ", "))
}
