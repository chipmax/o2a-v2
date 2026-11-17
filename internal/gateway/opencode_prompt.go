package gateway

import (
	_ "embed"
	"runtime"
	"strings"
	"time"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// The free tier accepts only agent-shaped requests, and its abuse checks expect
// the system prompt the official CLI sends. These assets are the CLI's own
// wording: the v1 text for /zen (OpenCode 1.x tool names) and the v2 text for
// /inference and /zen/go (OpenCode 2.x tool names). The v2 file is the v1 file
// with the tool names the 2.x CLI uses.
//
//go:embed opencode_prompt_v1.txt
var opencodePromptV1 string

//go:embed opencode_prompt_v2.txt
var opencodePromptV2 string

// opencodePromptMarker identifies a body that already carries the official CLI
// system prompt, so a client that sends it itself is left alone.
const opencodePromptMarker = "You are opencode, an interactive CLI tool"

// opencodeSkills is the built-in skill listing the CLI appends after its
// environment block. It is static, so it is part of the injected prompt.
const opencodeSkills = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location>&lt;built-in&gt;</location>
  </skill>
</available_skills>`

// opencodeEnvironment renders the <env> block the CLI puts after its base
// prompt. The free-tier gate requires an <env> element that states a working
// directory: an otherwise perfect request is refused with the quota-shaped 429
// when the line is missing, so the block keeps it. The path is a fixed neutral
// one rather than the service account's real directory — the gate checks the
// line, not the path, and the host filesystem must not leak to the model.
func opencodeEnvironment(model string, environment *config.PromptEnvironmentConfig) string {
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	directory, workspace, gitRepo := "/workspace", "/workspace", "yes"
	if environment != nil {
		if environment.WorkingDirectory != "" {
			directory = environment.WorkingDirectory
			workspace = directory
		}
		if environment.WorkspaceRoot != "" {
			workspace = environment.WorkspaceRoot
		}
		if environment.Platform != "" {
			platform = environment.Platform
		}
		if environment.IsGitRepo != nil && !*environment.IsGitRepo {
			gitRepo = "no"
		}
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + directory,
		"  Workspace root folder: " + workspace,
		"  Is directory a git repo: " + gitRepo,
		"  Platform: " + platform,
		"  Today's date: " + time.Now().UTC().Format("Mon Jan 02 2006"),
		"</env>",
	}, "\n")
}

// opencodeSystemPrompt assembles the full CLI system prompt for model and the
// upstream generation's tool naming.
func opencodeSystemPrompt(model string, v1 bool, environment *config.PromptEnvironmentConfig) string {
	base := opencodePromptV2
	if v1 {
		base = opencodePromptV1
	}
	return base + opencodeEnvironment(model, environment) + "\n" + opencodeSkills
}

// injectOfficialPrompt prepends the official CLI system prompt to a shaped
// free-tier payload. It reports whether the payload changed; a body that
// already carries the marker is left untouched, so repeated shaping cannot
// duplicate the prompt. System One payloads are not chat bodies and are never
// touched.
func injectOfficialPrompt(payload map[string]any, protocol wire.Protocol, v1 bool, environment *config.PromptEnvironmentConfig) bool {
	if payload == nil || protocol == wire.SystemOne {
		return false
	}
	model, _ := payload["model"].(string)
	prompt := opencodeSystemPrompt(model, v1, environment)
	switch protocol {
	case wire.Chat:
		return injectChatPrompt(payload, prompt)
	case wire.Anthropic:
		return injectAnthropicPrompt(payload, prompt)
	case wire.Responses:
		return injectResponsesPrompt(payload, prompt)
	default:
		return false
	}
}

// injectChatPrompt folds every system/developer message into the single system
// message the 1.x and 2.x CLIs send, then prepends it. Client instructions keep
// their order and are separated from the official prompt by a blank line.
func injectChatPrompt(payload map[string]any, prompt string) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(messages)+1)
	var instructions []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			kept = append(kept, raw)
			continue
		}
		content := message["content"]
		if carriesOpenCodePrompt(content) {
			return false
		}
		for _, part := range textFragments(content) {
			// The upstream reads only the first <env> pair. Our complete block
			// is prepended, so a stale client block must not shadow it.
			part = strings.TrimSpace(stripEnvBlocks(part))
			if part != "" {
				instructions = append(instructions, part)
			}
		}
	}
	system := prompt
	if joined := strings.Join(instructions, "\n"); joined != "" {
		system += "\n\n" + joined
	}
	payload["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, kept...)
	return true
}

// injectAnthropicPrompt prepends the prompt to the top-level system value,
// keeping a caller's string or block list intact after it.
func injectAnthropicPrompt(payload map[string]any, prompt string) bool {
	system, exists := payload["system"]
	if exists && carriesOpenCodePrompt(system) {
		return false
	}
	if !exists {
		payload["system"] = prompt
		return true
	}
	switch value := system.(type) {
	case string:
		value = strings.TrimSpace(stripEnvBlocks(value))
		if value == "" {
			payload["system"] = prompt
		} else {
			payload["system"] = prompt + "\n\n" + value
		}
	case []any:
		blocks := make([]any, 0, len(value))
		for _, block := range value {
			if item, ok := block.(map[string]any); ok {
				if text, ok := item["text"].(string); ok {
					item["text"] = stripEnvBlocks(text)
				}
			}
			blocks = append(blocks, block)
		}
		payload["system"] = append([]any{map[string]any{"type": "text", "text": prompt}}, blocks...)
	default:
		payload["system"] = prompt
	}
	return true
}

// injectResponsesPrompt prepends the prompt to the Responses instructions.
func injectResponsesPrompt(payload map[string]any, prompt string) bool {
	instructions, exists := payload["instructions"]
	if exists && carriesOpenCodePrompt(instructions) {
		return false
	}
	if text, ok := instructions.(string); exists && ok {
		text = strings.TrimSpace(stripEnvBlocks(text))
		if text == "" {
			payload["instructions"] = prompt
		} else {
			payload["instructions"] = prompt + "\n\n" + text
		}
		return true
	}
	payload["instructions"] = prompt
	return true
}

// stripEnvBlocks removes every <env>...</env> pair from caller-supplied text.
// The upstream reads only the first pair, so a stale or partial copy above the
// gateway's own complete block would shadow it and answer 429 as if the egress
// quota were spent. Only caller text is cleaned; the official prompt keeps its
// block.
func stripEnvBlocks(text string) string {
	for {
		start := strings.Index(text, "<env>")
		if start < 0 {
			return text
		}
		end := strings.Index(text[start:], "</env>")
		if end < 0 {
			// A lone opening tag can also shadow the real pair; drop the tail.
			return text[:start]
		}
		text = text[:start] + text[start+end+len("</env>"):]
	}
}

// textFragments flattens a system or instruction value into its text: a plain
// string, a list of content blocks, or a block object carrying text or content.
func textFragments(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, textFragments(item)...)
		}
		return parts
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return textFragments(text)
		}
		if content, ok := typed["content"]; ok {
			return textFragments(content)
		}
	}
	return nil
}

// carriesOpenCodePrompt reports whether any text inside value already contains
// the marker.
func carriesOpenCodePrompt(value any) bool {
	for _, part := range textFragments(value) {
		if strings.Contains(part, opencodePromptMarker) {
			return true
		}
	}
	return false
}
