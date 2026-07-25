package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitAgentFrontmatter(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		fm, body, err := splitAgentFrontmatter("---\nname: x\n---\nhello body\n")
		require.NoError(t, err)
		assert.Equal(t, "name: x", fm)
		assert.Equal(t, "hello body\n", body)
	})
	t.Run("leading blank lines and CRLF", func(t *testing.T) {
		fm, body, err := splitAgentFrontmatter("\r\n\r\n---\r\nname: x\r\n---\r\nbody\r\n")
		require.NoError(t, err)
		assert.Equal(t, "name: x", fm)
		assert.Equal(t, "body\n", body)
	})
	t.Run("no frontmatter", func(t *testing.T) {
		_, _, err := splitAgentFrontmatter("just text, no fences")
		require.Error(t, err)
	})
	t.Run("unclosed frontmatter", func(t *testing.T) {
		_, _, err := splitAgentFrontmatter("---\nname: x\nbody without close")
		require.Error(t, err)
	})
}

func TestToCrushTools(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"claude names mapped", []string{"Read", "Grep", "Glob", "Bash"}, []string{"view", "grep", "glob", "bash"}},
		{"native names pass through", []string{"view", "edit", "multiedit"}, []string{"view", "edit", "multiedit"}},
		{"webfetch and todowrite", []string{"WebFetch", "TodoWrite"}, []string{"fetch", "todos"}},
		{"unknown dropped", []string{"Read", "Telepathy"}, []string{"view"}},
		{"agent stripped", []string{"Read", "agent"}, []string{"view"}},
		{"agentic_fetch stripped", []string{"agentic_fetch", "Bash"}, []string{"bash"}},
		{"claude Task maps to agent then stripped", []string{"Task", "Read"}, []string{"view"}},
		{"dedup", []string{"Read", "view", "Read"}, []string{"view"}},
		{"empty", []string{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, toCrushTools(tt.in))
		})
	}
}

func TestToModelType(t *testing.T) {
	assert.Equal(t, SelectedModelTypeLarge, toModelType(""))
	assert.Equal(t, SelectedModelTypeLarge, toModelType("large"))
	assert.Equal(t, SelectedModelTypeLarge, toModelType("LARGE"))
	assert.Equal(t, SelectedModelTypeSmall, toModelType("small"))
	assert.Equal(t, SelectedModelTypeSmall, toModelType("Small"))
	// Claude aliases and unknowns fall back to large.
	assert.Equal(t, SelectedModelTypeLarge, toModelType("sonnet"))
	assert.Equal(t, SelectedModelTypeLarge, toModelType("haiku"))
	assert.Equal(t, SelectedModelTypeLarge, toModelType("inherit"))
}

func writeAgentFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestParseCustomAgent(t *testing.T) {
	inherit := allToolNames()

	t.Run("full file with explicit tools and skills", func(t *testing.T) {
		dir := t.TempDir()
		path := writeAgentFile(t, dir, "reviewer.md", `---
name: code-reviewer
description: Reviews code for bugs.
tools: Read, Grep, Bash
model: small
skills: [deep-research, secret-skill]
---
You are a reviewer.
`)
		agent, err := parseCustomAgent(path, inherit)
		require.NoError(t, err)
		assert.Equal(t, "code-reviewer", agent.ID)
		assert.Equal(t, "code-reviewer", agent.Name)
		assert.Equal(t, "Reviews code for bugs.", agent.Description)
		assert.Equal(t, SelectedModelTypeSmall, agent.Model)
		assert.Equal(t, []string{"view", "grep", "bash"}, agent.AllowedTools)
		assert.Equal(t, []string{"deep-research", "secret-skill"}, agent.Skills)
		assert.Equal(t, "You are a reviewer.", agent.SystemPrompt)
		assert.Equal(t, path, agent.Source)
	})

	t.Run("tools omitted inherits minus sub-agent tools", func(t *testing.T) {
		dir := t.TempDir()
		path := writeAgentFile(t, dir, "a.md", `---
name: helper
description: Helps.
---
body
`)
		agent, err := parseCustomAgent(path, inherit)
		require.NoError(t, err)
		assert.NotContains(t, agent.AllowedTools, "agent")
		assert.NotContains(t, agent.AllowedTools, "agentic_fetch")
		assert.Contains(t, agent.AllowedTools, "bash")
		assert.Contains(t, agent.AllowedTools, "view")
		assert.Equal(t, SelectedModelTypeLarge, agent.Model)
	})

	t.Run("missing name is rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := writeAgentFile(t, dir, "a.md", "---\ndescription: x\n---\nbody")
		_, err := parseCustomAgent(path, inherit)
		require.Error(t, err)
	})

	t.Run("missing description is rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := writeAgentFile(t, dir, "a.md", "---\nname: ok\n---\nbody")
		_, err := parseCustomAgent(path, inherit)
		require.Error(t, err)
	})

	t.Run("invalid name is rejected", func(t *testing.T) {
		dir := t.TempDir()
		path := writeAgentFile(t, dir, "a.md", "---\nname: Bad Name!\ndescription: x\n---\nbody")
		_, err := parseCustomAgent(path, inherit)
		require.Error(t, err)
	})
}

func TestDiscoverCustomAgents(t *testing.T) {
	inherit := allToolNames()

	t.Run("loads valid, skips reserved and malformed", func(t *testing.T) {
		dir := t.TempDir()
		writeAgentFile(t, dir, "reviewer.md", "---\nname: reviewer\ndescription: r\n---\nbody")
		writeAgentFile(t, dir, "researcher.md", "---\nname: researcher\ndescription: x\n---\nbody")
		// Reserved id - must be skipped.
		writeAgentFile(t, dir, "task.md", "---\nname: task\ndescription: nope\n---\nbody")
		// Malformed - must be skipped, not fatal.
		writeAgentFile(t, dir, "broken.md", "no frontmatter here")
		// Non-markdown - ignored.
		writeAgentFile(t, dir, "notes.txt", "irrelevant")

		agents := discoverCustomAgents([]string{dir}, inherit)
		require.Len(t, agents, 2)
		// Sorted by id.
		assert.Equal(t, "researcher", agents[0].ID)
		assert.Equal(t, "reviewer", agents[1].ID)
	})

	t.Run("last-wins across dirs (project overrides global)", func(t *testing.T) {
		globalDir := t.TempDir()
		projectDir := t.TempDir()
		writeAgentFile(t, globalDir, "dup.md", "---\nname: dup\ndescription: global\n---\nglobal body")
		writeAgentFile(t, projectDir, "dup.md", "---\nname: dup\ndescription: project\n---\nproject body")

		// Global passed before project => project wins.
		agents := discoverCustomAgents([]string{globalDir, projectDir}, inherit)
		require.Len(t, agents, 1)
		assert.Equal(t, "project", agents[0].Description)
		assert.Equal(t, "project body", agents[0].SystemPrompt)
	})

	t.Run("missing dirs are ignored", func(t *testing.T) {
		agents := discoverCustomAgents([]string{filepath.Join(t.TempDir(), "does-not-exist")}, inherit)
		assert.Empty(t, agents)
	})
}
