package toolcall

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"aurora/typings/official"
)

// ShellToolNames 候选 shell 工具名,按顺序匹配第一个在 tools 列表里出现的。
var ShellToolNames = []string{
	"bash", "shell", "run_command", "execute_command",
	"terminal", "powershell", "run", "command",
}

// ShellParamCandidates 参数名候选(command / cmd / script / input),
// 按出现顺序挑第一个在工具 schema 中声明的。
var ShellParamCandidates = []string{"command", "cmd", "script", "input"}

// ResolveShellTool 找到 tools 列表里第一个 shell 工具,返回 (name, param)。
// 没找到就返回 ("", "")。
func ResolveShellTool(tools []official.Tool) (string, string) {
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		name := strings.ToLower(t.Function.Name)
		if !contains(ShellToolNames, name) {
			continue
		}
		param := pickShellParam(t.Function.Parameters)
		return t.Function.Name, param
	}
	return "", ""
}

var (
	fencedCommandPattern = regexp.MustCompile("(?is)```(?:bash|sh|shell|powershell|pwsh|cmd)?\\s*\\r?\\n?(.+?)```")
	inlineCommandPattern = regexp.MustCompile("`([^`\\r\\n]+)`")
	englishRunPattern    = regexp.MustCompile(`(?is)\b(?:run|execute)\s+(?:(?:the\s+)?command\s+)?(.+)$`)
	vietnameseRunPattern = regexp.MustCompile(`(?is)\b(?:chạy|chay)\s+(?:lệnh|lenh)\s+(.+)$`)
)

// RecoverExplicitShellRequest builds a host shell tool call without an
// upstream model round-trip when the user supplied the literal command. It is
// deliberately strict: fenced/inline code is accepted, as is text following
// an explicit "run/execute command" phrase. Vague requests are left to the
// model instead of inventing a command.
func RecoverExplicitShellRequest(text string, tools []official.Tool) []official.ToolCall {
	shellName, shellParam := ResolveShellTool(tools)
	if shellName == "" {
		return nil
	}
	command := explicitCommandFromText(text, shellName)
	if command == "" {
		return nil
	}
	arguments, _ := json.Marshal(map[string]string{shellParam: command})
	return []official.ToolCall{{
		ID:   generateCallID(),
		Type: "function",
		Function: official.ToolCallFunc{
			Name:      shellName,
			Arguments: string(arguments),
		},
	}}
}

func explicitCommandFromText(text, shellName string) string {
	if match := fencedCommandPattern.FindStringSubmatch(text); len(match) == 2 {
		return cleanExplicitCommand(match[1], true)
	}
	inline := inlineCommandPattern.FindAllStringSubmatch(text, -1)
	inlineCandidates := make([]string, 0, len(inline))
	for i := 0; i < len(inline); i++ {
		candidate := cleanExplicitCommand(inline[i][1], false)
		if candidate != "" && !strings.EqualFold(candidate, shellName) {
			inlineCandidates = append(inlineCandidates, candidate)
		}
	}
	if len(inlineCandidates) == 1 {
		return inlineCandidates[0]
	}
	if len(inlineCandidates) > 1 {
		return ""
	}
	for _, pattern := range []*regexp.Regexp{vietnameseRunPattern, englishRunPattern} {
		if match := pattern.FindStringSubmatch(text); len(match) == 2 {
			if candidate := cleanExplicitCommand(trimCommandTail(match[1]), false); commandLooksLiteral(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func trimCommandTail(command string) string {
	lower := strings.ToLower(command)
	cut := len(command)
	for _, separator := range []string{
		" and then ", " then ", " and reply", " and return", " on the real machine", " on the host",
		" rồi ", " roi ", " và trả lời", " va tra loi", " trên máy", " tren may",
	} {
		if i := strings.Index(lower, separator); i >= 0 && i < cut {
			cut = i
		}
	}
	return command[:cut]
}

func cleanExplicitCommand(command string, multiline bool) string {
	command = strings.TrimSpace(command)
	command = strings.Trim(command, `"'`)
	if !multiline && strings.ContainsAny(command, "\r\n") {
		return ""
	}
	if strings.HasSuffix(command, ".") && !strings.HasSuffix(command, "...") {
		command = strings.TrimSpace(strings.TrimSuffix(command, "."))
	}
	if len(command) == 0 || len(command) > 4096 {
		return ""
	}
	return command
}

func commandLooksLiteral(command string) bool {
	if command == "" {
		return false
	}
	first := strings.ToLower(strings.Fields(command)[0])
	for _, vague := range []string{"a", "an", "the", "this", "that", "some", "command", "tool", "tests", "test", "code"} {
		if first == vague {
			return false
		}
	}
	for _, r := range first {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune(`._:/\\-`, r) {
			continue
		}
		return false
	}
	return true
}

func pickShellParam(paramsJSON json.RawMessage) string {
	if len(paramsJSON) == 0 {
		return "command"
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(paramsJSON, &schema); err != nil || schema.Properties == nil {
		return "command"
	}
	for _, cand := range ShellParamCandidates {
		if _, ok := schema.Properties[cand]; ok {
			return cand
		}
	}
	return "command"
}

// RecoverFromText 兜底:从已输出的纯文本里扫描 JSON 对象,抽出
// (a) {"name":..., "arguments":...} 或
// (b) {"cmd": [...]} / {"cmd": "..."} 这两种 chatgpt 沙箱形状
// 然后转成 ToolCall。返回的列表已去重。
func RecoverFromText(text string, tools []official.Tool) []official.ToolCall {
	if !strings.Contains(text, "{") {
		return nil
	}
	shellName, shellParam := ResolveShellTool(tools)
	seen := make(map[string]bool)
	var out []official.ToolCall
	for _, obj := range iterJSONObjects(text) {
		var tc *official.ToolCall
		if name := pickString(obj, "name", "tool", "tool_name", "function"); name != "" {
			tc = buildToolCallFromObject(obj)
		} else if shellName != "" {
			if cmd, ok := obj["cmd"]; ok {
				if s := cmdToString(cmd); s != "" {
					raw, _ := json.Marshal(map[string]string{shellParam: s})
					tc = &official.ToolCall{
						ID:   generateCallID(),
						Type: "function",
						Function: official.ToolCallFunc{
							Name:      shellName,
							Arguments: string(raw),
						},
					}
				}
			}
		}
		if tc == nil {
			continue
		}
		key := tc.Function.Name + "\x00" + tc.Function.Arguments
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, *tc)
	}
	return out
}

// iterJSONObjects 扫描 text 中所有花括号平衡的 JSON 对象。
// 按出现顺序返回(只对顶层对象计数,字符串里的 {} 不算)。
func iterJSONObjects(text string) []map[string]any {
	var out []map[string]any
	depth := 0
	inStr := false
	esc := false
	start := -1
	for i := 0; i < len(text); i++ {
		c := text[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				blob := text[start : i+1]
				if v, ok := robustJSON(blob); ok {
					out = append(out, v)
				}
				start = -1
			}
		}
	}
	return out
}

// cmdToString 把 cmd 字段归一化成单行命令:
//   - "ls -la"           → "ls -la"
//   - ["bash","-c","ls"] → "ls"
//   - ["powershell","-Command","Get-Process"] → "Get-Process"
func cmdToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		parts := make([]string, len(t))
		for i, p := range t {
			parts[i] = fmt.Sprint(p)
		}
		low := make([]string, len(parts))
		for i, p := range parts {
			low[i] = strings.ToLower(p)
		}
		// 解开 -Command / -c 包装
		for i, p := range low {
			if (p == "-command" || p == "-c") && i+1 < len(parts) {
				rest := parts[i+1:]
				if len(rest) == 1 {
					return rest[0]
				}
				return strings.Join(rest, " ")
			}
		}
		// 去掉 shell 可执行名
		if len(low) > 0 && contains([]string{"powershell", "pwsh", "cmd", "bash", "sh"}, low[0]) {
			return strings.Join(parts[1:], " ")
		}
		return strings.Join(parts, " ")
	case []string:
		parts := make([]string, len(t))
		copy(parts, t)
		low := make([]string, len(parts))
		for i, p := range parts {
			low[i] = strings.ToLower(p)
		}
		for i, p := range low {
			if (p == "-command" || p == "-c") && i+1 < len(parts) {
				rest := parts[i+1:]
				if len(rest) == 1 {
					return rest[0]
				}
				return strings.Join(rest, " ")
			}
		}
		if len(low) > 0 && contains([]string{"powershell", "pwsh", "cmd", "bash", "sh"}, low[0]) {
			return strings.Join(parts[1:], " ")
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// StreamToToolCallDeltas 把完整 ToolCall 列表切成 OpenAI 流式协议需要的
// 多个 delta chunk(按出现顺序):
//  1. 先发 {index, id, type, function.name}
//  2. 再发 {index, function.arguments(分片)}
//
// 返回的每个元素对应一个 SSE chunk 的 delta.tool_calls 数组(每条只有一项)。
func StreamToToolCallDeltas(calls []official.ToolCall) [][]official.ToolCallDelta {
	var out [][]official.ToolCallDelta
	for i, c := range calls {
		c.Index = i
		out = append(out, []official.ToolCallDelta{{
			Index: i,
			ID:    c.ID,
			Type:  c.Type,
			Function: official.ToolCallFuncDelta{
				Name: c.Function.Name,
			},
		}})
		if c.Function.Arguments != "" {
			out = append(out, []official.ToolCallDelta{{
				Index: i,
				Function: official.ToolCallFuncDelta{
					Arguments: c.Function.Arguments,
				},
			}})
		}
	}
	return out
}

// SerializeForHistory 把历史的 tool_calls 列表反序列化为 <tool_call>{...}<tool_call>
// 文本,用于回放到 prompt 中。
func SerializeForHistory(calls []official.ToolCallRef) string {
	var sb strings.Builder
	// 按 index 升序遍历,避免乱序
	sorted := make([]official.ToolCallRef, len(calls))
	copy(sorted, calls)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	for _, c := range sorted {
		sb.WriteString("\n")
		sb.WriteString(StartTag)
		sb.WriteString(`{"name": "`)
		sb.WriteString(c.Function.Name)
		sb.WriteString(`", "arguments": `)
		sb.WriteString(c.Function.Arguments)
		sb.WriteString(`}`)
		sb.WriteString(EndTag)
	}
	return sb.String()
}
