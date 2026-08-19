package toolcall

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"aurora/typings/official"
)

// BuildInstructions 生成 system prompt 块,教导模型按 <tool_call>{...}</tool_call>
// 协议输出工具调用。tools 为空时返回 ""。
// 协议文本与  保持一致,但使用英语(目标用户以英文/中文为主)。
func BuildInstructions(tools []official.Tool, toolChoice *official.ToolChoice) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("# TOOLS AVAILABLE\n")
	sb.WriteString("These tools run on the user's host. Use only the exact names and parameters listed here.\n")
	sb.WriteString(compactToolsPrompt(tools))
	sb.WriteString("\n# TOOL CALLING FORMAT\n")
	sb.WriteString(`<tool_call>{"name":"exact_tool_name","arguments":{"param":"value"}}</tool_call>`)
	sb.WriteString("\nUse valid JSON and include arguments. Emit one block per call; independent calls may be consecutive. When calling tools, output only tool-call blocks with no prose. Never use an internal sandbox in place of these host tools.\n")
	if forced := toolChoice.ForcedFunctionName(); forced != "" {
		fmt.Fprintf(&sb, "MUST call the tool %q now; do not call another tool or answer first.\n", forced)
	} else if toolChoice != nil && toolChoice.IsForcedNone() {
		sb.WriteString("DISABLED tool calling: answer in plain text and emit no tool-call block.\n")
	} else if toolChoice != nil && toolChoice.RequiresCall() {
		sb.WriteString("MUST call at least one listed tool now; do not answer first.\n")
	} else {
		sb.WriteString("Tools are optional. For an informational question that needs no host access, answer normally without a tool call.\n")
	}
	return sb.String()
}

// compactToolsPrompt 把工具列表渲染成人可读的多行描述。
func compactToolsPrompt(tools []official.Tool) string {
	var sb strings.Builder
	for _, t := range tools {
		if t.Type != "function" {
			sb.WriteString("- ")
			// 非 function 工具:原样 JSON 化
			b, _ := json.Marshal(t)
			sb.Write(b)
			sb.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&sb, "- %s: %s\n", t.Function.Name, compactPromptText(t.Function.Description, 240))
		var schema struct {
			Type       string                    `json:"type"`
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
		}
		if len(t.Function.Parameters) == 0 {
			continue
		}
		if err := json.Unmarshal(t.Function.Parameters, &schema); err != nil || schema.Type != "object" || len(schema.Properties) == 0 {
			continue
		}
		sb.WriteString("  Params:\n")
		// 排序以保证稳定输出(便于测试和 prompt 缓存)
		keys := make([]string, 0, len(schema.Properties))
		for k := range schema.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			prop := schema.Properties[key]
			isReq := "optional"
			for _, r := range schema.Required {
				if r == key {
					isReq = "required"
					break
				}
			}
			desc, _ := prop["description"].(string)
			desc = compactPromptText(desc, 160)
			typeStr, _ := prop["type"].(string)
			if typeStr == "" {
				typeStr = "string"
			}
			if enum, ok := prop["enum"].([]any); ok {
				var opts []string
				for _, e := range enum {
					opts = append(opts, fmt.Sprint(e))
				}
				if desc != "" {
					desc += " "
				}
				desc += "Options: [" + strings.Join(opts, ", ") + "]"
			}
			if desc != "" {
				fmt.Fprintf(&sb, "    * %s (%s, %s): %s\n", key, typeStr, isReq, desc)
			} else {
				fmt.Fprintf(&sb, "    * %s (%s, %s)\n", key, typeStr, isReq)
			}
		}
	}
	return sb.String()
}

// compactPromptText keeps client-provided descriptions useful without letting
// verbose SDK metadata dominate every upstream turn. Limits are rune-based so
// multilingual descriptions are not split in the middle of a UTF-8 sequence.
func compactPromptText(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(text), " ")
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return strings.TrimSpace(string(runes[:maxRunes])) + "…"
}

// FirstToolCallExample 根据 tools 列表的语义,生成一个具体的"先做这个"示例,
// 帮模型跳出 sandbox 思维。优先级:bash/shell → glob → list_files → read → 任意。
// workingDir 用于替换占位符;若为空用 "."。
func FirstToolCallExample(tools []official.Tool, workingDir string) string {
	if len(tools) == 0 {
		return ""
	}
	wd := strings.ReplaceAll(workingDir, `\`, `\\`)
	if wd == "" {
		wd = "."
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Type == "function" {
			names = append(names, t.Function.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	if n := pickFirst(names, ShellToolNames); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"command": "Get-ChildItem -Force"}}</tool_call>`, n)
	}
	if n := pickFirst(names, []string{"glob", "find", "search_files", "file_search"}); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"pattern": "*"}}</tool_call>`, n)
	}
	if n := pickFirst(names, []string{"list", "ls", "read_directory", "list_files", "list_directory"}); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"path": %q}}</tool_call>`, n, wd)
	}
	if n := pickFirst(names, []string{"read", "read_file", "cat", "open", "view"}); n != "" {
		return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {"filePath": %q}}</tool_call>`, n, wd)
	}
	return fmt.Sprintf(`<tool_call>{"name": %q, "arguments": {}}</tool_call>`, names[0])
}

// MutationToolsPrompt returns only the concrete host tools that can write or
// edit workspace content. It is used after the model has already decided that
// real host action is required, so this is protocol guidance rather than user
// intent classification.
func MutationToolsPrompt(tools []official.Tool) string {
	filtered := make([]official.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		name := strings.ToLower(t.Function.Name)
		if name == "write" || name == "edit" || name == "apply_patch" || name == "patch" || name == "write_file" || name == "create_file" || name == "str_replace" || name == "replace" || strings.Contains(name, "write") || strings.Contains(name, "edit") || strings.Contains(name, "patch") {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	return compactToolsPrompt(filtered)
}

func pickFirst(haystack []string, candidates []string) string {
	for _, h := range haystack {
		hl := strings.ToLower(h)
		for _, c := range candidates {
			if hl == c {
				return h
			}
		}
	}
	return ""
}

// ExtractWorkingDir 从 messages 中扫描 "Working directory: X" 模式,
// Kilo/OpenCode 等客户端会在 user 消息里塞这个 hint。
func ExtractWorkingDir(messages []official.APIMessage) string {
	for _, m := range messages {
		for _, line := range strings.Split(m.Content.Text(), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Working directory:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "Working directory:"))
			}
		}
	}
	return ""
}

// FinalNudge 是给模型末尾追加的"先做这个,别分析 sandbox"系统指令。
// 用 lastRole 决定上下文:
//   - tool   : 提醒把 tool 输出当 ground truth,继续调用或总结
//   - user   : 强制模型立刻发 <tool_call>(不思考、不描述环境)
//   - 其他   : 返回空
func FinalNudge(tools []official.Tool, messages []official.APIMessage, toolChoice *official.ToolChoice) string {
	if len(tools) == 0 || len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	switch last.Role {
	case "tool", "function":
		// 拿不到具体的 tool 名(API 没有 tool_call_id 映射),用一个通用表达
		return "\n[SYSTEM: The Tool block above is real host output and ground truth. Use it to answer, or emit another exact <tool_call> block only if more host work is needed.]"
	case "user":
		forced := toolChoice != nil && toolChoice.RequiresCall()
		if !forced {
			return ""
		}
		wd := ExtractWorkingDir(messages)
		example := FirstToolCallExample(tools, wd)
		wdPart := ""
		if wd != "" {
			wdPart = fmt.Sprintf(" (working directory: %s)", wd)
		}
		var sb strings.Builder
		sb.WriteString("\n[SYSTEM: Host access is available only through the listed <tool_call> protocol")
		sb.WriteString(wdPart)
		sb.WriteString(". Reply now with only one or more valid <tool_call> blocks; no prose and no guessed host state.\n")
		if example != "" {
			sb.WriteString("Valid shape: ")
			sb.WriteString(example)
		}
		sb.WriteString("]\n")
		return sb.String()
	}
	return ""
}
