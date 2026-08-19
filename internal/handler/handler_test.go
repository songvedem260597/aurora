package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/internal/accounts"
	"aurora/internal/config"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ─── Test: writeChatCompletionStreamDone ─────────────────────────

func TestWriteChatCompletionStreamDoneAddsStopBeforeDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	writeChatCompletionStreamDone(c, false, "auto", "conv-xxx")

	lines := sseDataLines(writer.Body.String())
	if len(lines) != 2 {
		t.Fatalf("data line count = %d, want 2; output: %s", len(lines), writer.Body.String())
	}
	var stopChunk map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &stopChunk); err != nil {
		t.Fatalf("invalid stop chunk: %v", err)
	}
	if stopChunk["conversation_id"] != "conv-xxx" {
		t.Fatalf("conversation_id = %#v, want conv-xxx", stopChunk["conversation_id"])
	}
	choices := stopChunk["choices"].([]interface{})
	if choices[0].(map[string]interface{})["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v, want stop", choices[0].(map[string]interface{})["finish_reason"])
	}
	if lines[1] != "[DONE]" {
		t.Fatalf("last data line = %q, want [DONE]", lines[1])
	}
}

func TestWriteChatCompletionStreamDoneSkipsDuplicateStop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	writeChatCompletionStreamDone(c, true, "auto", "conv-xxx")

	lines := sseDataLines(writer.Body.String())
	if len(lines) != 1 || lines[0] != "[DONE]" {
		t.Fatalf("data lines = %#v, want only [DONE]", lines)
	}
}

// ─── Test: toolCallingEnabled ────────────────────────────────────

func TestToolCallingEnabledFromConfig(t *testing.T) {
	okCfg := &config.Config{ToolCallingEnabled: true}
	disabledCfg := &config.Config{ToolCallingEnabled: false}

	if toolCallingEnabled(nil, okCfg) {
		t.Error("toolCallingEnabled(nil, true) should be false (len(nil)==0)")
	}
	if toolCallingEnabled(nil, disabledCfg) {
		t.Error("toolCallingEnabled(nil, false) should be false")
	}
	// empty tools slice with config enabled → false
	if toolCallingEnabled([]officialtypes.Tool{}, okCfg) {
		t.Error("toolCallingEnabled([], true) should be false")
	}
	// with actual tools and config enabled → true
	tools := []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "test"}}}
	if !toolCallingEnabled(tools, okCfg) {
		t.Error("toolCallingEnabled([tool], true) should be true")
	}
}

func TestShouldRequireToolCallForDeferredWorkspaceAction(t *testing.T) {
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{{
			Role:    "user",
			Content: officialtypes.MessageContent{TextValue: "ok làm đi"},
		}},
	}
	text := "Tao bắt đầu bằng việc xem cấu trúc repo hiện tại để không đè nhầm code, sau đó sẽ sửa."
	if !shouldRequireToolCall(req, text) {
		t.Fatal("deferred workspace action should require an actual tool call")
	}
}

func TestShouldRequireToolCallForExplicitToolRequest(t *testing.T) {
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{{
			Role:    "user",
			Content: officialtypes.MessageContent{TextValue: "You must use the shell tool to print the current working directory."},
		}},
	}
	if !shouldRequireToolCall(req, "/") {
		t.Fatal("an explicit shell request must not degrade into plain text")
	}
}

func TestShouldRequireToolCallRespectsAutoPlainAnswer(t *testing.T) {
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{{
			Role:    "user",
			Content: officialtypes.MessageContent{TextValue: "Explain what a closure is."},
		}},
	}
	if shouldRequireToolCall(req, "A closure is a function together with its captured lexical environment.") {
		t.Fatal("tool_choice=auto must still allow an ordinary text answer")
	}
}

func TestShouldRequireToolCallRespectsForcedChoice(t *testing.T) {
	req := &officialtypes.APIRequest{
		Tools:      []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		ToolChoice: &officialtypes.ToolChoice{Type: "any"},
	}
	if !shouldRequireToolCall(req, "plain text") {
		t.Fatal("tool_choice=any must require a tool call")
	}

	req.ToolChoice = &officialtypes.ToolChoice{Type: "none"}
	if shouldRequireToolCall(req, "I will inspect the repo") {
		t.Fatal("tool_choice=none must disable forced tool retries")
	}
}

func TestConversationRequestsActionDirectInstruction(t *testing.T) {
	messages := []officialtypes.APIMessage{{
		Role:    "user",
		Content: officialtypes.MessageContent{TextValue: "sửa code rồi test đi"},
	}}
	if !conversationRequestsAction(messages) {
		t.Fatal("direct implementation request should require execution")
	}
}

func TestConversationRequestsActionFromConfirmation(t *testing.T) {
	messages := []officialtypes.APIMessage{
		{Role: "assistant", Content: officialtypes.MessageContent{TextValue: "Tao sẽ sửa handler rồi chạy test."}},
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "ok"}},
	}
	if !conversationRequestsAction(messages) {
		t.Fatal("confirmation after promised action should inherit execution intent")
	}
}

func TestConversationRequestsActionDoesNotForceExplanation(t *testing.T) {
	messages := []officialtypes.APIMessage{{
		Role:    "user",
		Content: officialtypes.MessageContent{TextValue: "giải thích closure là gì"},
	}}
	if conversationRequestsAction(messages) {
		t.Fatal("pure explanation should not require a workspace action")
	}
}

func TestConversationRequestsActionAfterOpenAIJSONRoundTrip(t *testing.T) {
	body := `{"model":"gpt-5-6-thinking","messages":[{"role":"assistant","content":"Tao sẽ sửa handler rồi chạy test để xác nhận."},{"role":"user","content":"ok"}],"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}]}`
	var req officialtypes.APIRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if !conversationRequestsAction(req.Messages) {
		t.Fatal("OpenAI JSON round-trip should preserve inherited execution intent")
	}
	if !shouldRequireToolCall(&req, "") {
		t.Fatal("parsed OpenAI request should require a tool call")
	}
}

func TestToolResultAllowsFollowupAnswer(t *testing.T) {
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "You must use bash to print pwd, then report the actual result."}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{{ID: "call_1", Type: "function"}}},
			{Role: "tool", ToolCallID: "call_1", Name: "bash", Content: officialtypes.MessageContent{TextValue: "/c/Users/test/project"}},
		},
	}
	if shouldRequireToolCall(req, "") {
		t.Fatal("a completed tool result must allow the model to answer instead of forcing another tool call")
	}
}

func TestMutationTaskRejectsReadOnlyToolResult(t *testing.T) {
	call := officialtypes.ToolCallRef{ID: "call_read", Type: "function"}
	call.Function.Name = "bash"
	call.Function.Arguments = `{"command":"Test-Path -LiteralPath C:\\Users\\uchih\\Desktop"}`
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "hãy tạo game pvz trên desktop bằng html đi"}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{call}},
			{Role: "tool", ToolCallID: "call_read", Content: officialtypes.MessageContent{TextValue: "True"}},
		},
	}
	if !shouldRequireToolCall(req, "Mình đang tạo game thật trên Desktop.") {
		t.Fatal("read-only Test-Path must not satisfy a create/modify task")
	}
}

func TestGenericMutationTaskAllowsFinalAfterWrite(t *testing.T) {
	call := officialtypes.ToolCallRef{ID: "call_write", Type: "function"}
	call.Function.Name = "write"
	call.Function.Arguments = `{"filePath":"C:\\Users\\uchih\\Desktop\\settings.txt","content":"enabled=true"}`
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "write"}}},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "sửa cài đặt đi"}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{call}},
			{Role: "tool", ToolCallID: "call_write", Content: officialtypes.MessageContent{TextValue: "written"}},
		},
	}
	if shouldRequireToolCall(req, "Đã cập nhật cài đặt.") {
		t.Fatal("a real write tool result should satisfy a generic mutation task")
	}
}

// ─── Test: original_requestHasFiles ──────────────────────────────

func TestContentTaskRejectsDirectoryOnlySetup(t *testing.T) {
	call := officialtypes.ToolCallRef{ID: "call_mkdir", Type: "function"}
	call.Function.Name = "bash"
	call.Function.Arguments = `{"command":"New-Item -ItemType Directory -Path C:\\Users\\uchih\\Desktop\\VoxelCraft"}`
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "tạo game minecraft trên desktop đi"}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{call}},
			{Role: "tool", ToolCallID: "call_mkdir", Content: officialtypes.MessageContent{TextValue: "C:\\Users\\uchih\\Desktop\\VoxelCraft"}},
		},
	}
	if !shouldRequireToolCall(req, "Mình sẽ làm game voxel 3D chạy trực tiếp trên desktop.") {
		t.Fatal("directory-only setup must not satisfy a coding/game task")
	}
}

func TestContentTaskRequiresVerificationAfterWrite(t *testing.T) {
	writeCall := officialtypes.ToolCallRef{ID: "call_write", Type: "function"}
	writeCall.Function.Name = "write"
	writeCall.Function.Arguments = `{"filePath":"C:\\Users\\uchih\\Desktop\\VoxelCraft\\index.html","content":"<html></html>"}`
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "write"}}},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "tạo game minecraft trên desktop đi"}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{writeCall}},
			{Role: "tool", ToolCallID: "call_write", Content: officialtypes.MessageContent{TextValue: "written"}},
		},
	}
	if !shouldRequireToolCall(req, "Đã tạo game xong.") {
		t.Fatal("coding task must verify after writing content")
	}
}

func TestContentTaskAllowsFinalAfterWriteAndVerification(t *testing.T) {
	writeCall := officialtypes.ToolCallRef{ID: "call_write", Type: "function"}
	writeCall.Function.Name = "write"
	writeCall.Function.Arguments = `{"filePath":"C:\\Users\\uchih\\Desktop\\VoxelCraft\\index.html","content":"<html></html>"}`
	verifyCall := officialtypes.ToolCallRef{ID: "call_verify", Type: "function"}
	verifyCall.Function.Name = "bash"
	verifyCall.Function.Arguments = `{"command":"Get-Content C:\\Users\\uchih\\Desktop\\VoxelCraft\\index.html | Select-Object -First 1"}`
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "tạo game minecraft trên desktop đi"}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{writeCall}},
			{Role: "tool", ToolCallID: "call_write", Content: officialtypes.MessageContent{TextValue: "written"}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{verifyCall}},
			{Role: "tool", ToolCallID: "call_verify", Content: officialtypes.MessageContent{TextValue: "<html></html>"}},
		},
	}
	if shouldRequireToolCall(req, "Đã tạo và kiểm tra game.") {
		t.Fatal("coding task with content mutation and later verification may finish")
	}
}

func TestBashMutationDetection(t *testing.T) {
	redirect := officialtypes.ToolCallRef{}
	redirect.Function.Name = "bash"
	redirect.Function.Arguments = `{"command":"printf GATE_OK > C:\\Users\\uchih\\Desktop\\probe.txt"}`
	if !toolCallMutatesWorkspace(redirect) {
		t.Fatal("shell redirection must count as mutation")
	}

	writeAllText := officialtypes.ToolCallRef{}
	writeAllText.Function.Name = "bash"
	writeAllText.Function.Arguments = `{"command":"powershell -Command [System.IO.File]::WriteAllText('probe.txt','ok')"}`
	if !toolCallMutatesWorkspace(writeAllText) {
		t.Fatal("WriteAllText must count as mutation")
	}
}
func TestOriginalRequestHasFiles(t *testing.T) {
	req := officialtypes.APIRequest{
		Messages: []officialtypes.APIMessage{
			{
				Role:    "user",
				Content: officialtypes.MessageContent{TextValue: "hello"},
			},
		},
	}
	if original_requestHasFiles(req) {
		t.Error("should be false when no files")
	}
}

// ─── Test: countMessagesTokens ───────────────────────────────────

func TestCountMessagesTokens(t *testing.T) {
	zero := countMessagesTokens(nil)
	if zero != 0 {
		t.Errorf("nil messages should return 0, got %d", zero)
	}
}

// ─── Test: resolveAccount ────────────────────────────────────────

func TestResolveAccountEmptyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pool := accounts.NewPool(nil)
	cfg := &config.Config{}

	acct, _, err := resolveAccount(c, pool, cfg, false)
	if err == nil {
		t.Fatal("expected error with empty pool")
	}
	if acct != nil {
		t.Fatal("expected nil account")
	}
}

func TestResolveAccountWithGlobalKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer my-global-key")

	pool := accounts.NewPool(nil)
	acct := accounts.NewAccount("test", accounts.TypeFree, "test-token")
	acct.Status = accounts.StatusActive
	pool.AddAccount(acct)
	cfg := &config.Config{Authorization: "my-global-key"}

	result, _, err := resolveAccount(c, pool, cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected account, got nil")
	}
	if result.Token != "test-token" {
		t.Errorf("got token %q, want test-token", result.Token)
	}
}

// ─── helpers ─────────────────────────────────────────────────────

func sseDataLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		lines = append(lines, strings.TrimPrefix(line, "data: "))
	}
	return lines
}
