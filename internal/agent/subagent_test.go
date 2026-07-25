package agent

import (
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/agent/prompt"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/skills"
)

// TestSubagentTemplateInjectsBodyVerbatim proves the load-bearing property
// that a custom agent's body is injected as template DATA (not re-parsed),
// so literal braces in user prose survive rendering intact. It renders the
// real embedded template the same way prompt.Build does.
func TestSubagentTemplateInjectsBodyVerbatim(t *testing.T) {
	tmpl, err := template.New("subagent").Parse(string(subagentPromptTmpl))
	require.NoError(t, err)

	var sb strings.Builder
	err = tmpl.Execute(&sb, prompt.PromptDat{
		AgentBody:       "Use {{this}} and {single} braces literally.",
		LoadedSkillsXML: "<loaded_skill>x</loaded_skill>",
		WorkingDir:      "/tmp/proj",
		Platform:        "darwin",
		Date:            "1/2/2006",
	})
	require.NoError(t, err)

	out := sb.String()
	assert.Contains(t, out, "Use {{this}} and {single} braces literally.")
	assert.Contains(t, out, "<loaded_skill>x</loaded_skill>")
	assert.Contains(t, out, "Working directory: /tmp/proj")
}

func TestResolveAgentSkills(t *testing.T) {
	c := &coordinator{
		allSkills: []*skills.Skill{
			{Name: "deep-research", Description: "Research deeply", SkillFilePath: "/s/deep-research/SKILL.md", Instructions: "do research"},
			{Name: "other", Description: "Other", SkillFilePath: "/s/other/SKILL.md", Instructions: "do other"},
		},
	}

	t.Run("no declared skills", func(t *testing.T) {
		assert.Empty(t, c.resolveAgentSkills(config.Agent{ID: "a"}))
	})

	t.Run("known skill is injected", func(t *testing.T) {
		xml := c.resolveAgentSkills(config.Agent{ID: "a", Skills: []string{"deep-research"}})
		assert.Contains(t, xml, "<loaded_skill>")
		assert.Contains(t, xml, "deep-research")
		assert.Contains(t, xml, "do research")
		// The unrelated skill must not leak in.
		assert.NotContains(t, xml, "do other")
	})

	t.Run("unknown skill is skipped, not fatal", func(t *testing.T) {
		xml := c.resolveAgentSkills(config.Agent{ID: "a", Skills: []string{"deep-research", "nope"}})
		assert.Contains(t, xml, "deep-research")
		assert.Equal(t, 1, strings.Count(xml, "<loaded_skill>"))
	})

	t.Run("only unknown skills yields empty", func(t *testing.T) {
		assert.Empty(t, c.resolveAgentSkills(config.Agent{ID: "a", Skills: []string{"nope"}}))
	})
}

func TestBuildAgentToolDescription(t *testing.T) {
	t.Run("no sub-agents returns base description", func(t *testing.T) {
		assert.Equal(t, agentToolDescription, buildAgentToolDescription(nil))
	})

	t.Run("enumerates sub-agents sorted", func(t *testing.T) {
		desc := buildAgentToolDescription(map[string]subAgentEntry{
			"zeta":          {description: "Z agent"},
			"code-reviewer": {description: "Reviews code"},
		})
		assert.Contains(t, desc, agentToolDescription)
		assert.Contains(t, desc, "code-reviewer: Reviews code")
		assert.Contains(t, desc, "zeta: Z agent")
		// code-reviewer (sorted first) appears before zeta.
		assert.Less(t, strings.Index(desc, "code-reviewer"), strings.Index(desc, "zeta"))
	})
}

func TestUnknownSubagentMessage(t *testing.T) {
	t.Run("lists available", func(t *testing.T) {
		msg := unknownSubagentMessage("ghost", map[string]subAgentEntry{
			"code-reviewer": {description: "x"},
			"researcher":    {description: "y"},
		})
		assert.Contains(t, msg, "ghost")
		assert.Contains(t, msg, "code-reviewer")
		assert.Contains(t, msg, "researcher")
	})

	t.Run("no custom agents configured", func(t *testing.T) {
		msg := unknownSubagentMessage("ghost", map[string]subAgentEntry{})
		assert.Contains(t, msg, "ghost")
		assert.Contains(t, msg, "no custom sub-agents")
	})
}
