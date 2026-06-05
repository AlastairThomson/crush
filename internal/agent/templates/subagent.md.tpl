{{ .AgentBody }}
{{ if .LoadedSkillsXML }}
The following skills have been pre-loaded for you. Follow their instructions to
complete the task; you do not need to discover or load them yourself.

{{ .LoadedSkillsXML }}
{{ end }}
<env>
Working directory: {{ .WorkingDir }}
Is directory a git repo: {{ if .IsGitRepo }} yes {{ else }} no {{ end }}
Platform: {{ .Platform }}
Today's date: {{ .Date }}
</env>
