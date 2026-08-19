package chatgpt

import (
	"aurora/httpclient"
	"aurora/internal/accounts"
	chatgpt_types "aurora/typings/chatgpt"
	"aurora/typings/official"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

var testAccount = accounts.NewAccount("test", accounts.TypeNoAuth, "")

func testConvert(t *testing.T, req official.APIRequest) chatgpt_types.ChatGPTRequest {
	t.Helper()
	return ConvertAPIRequest(req, testAccount, "", nil)
}

func TestConvertAPIRequestNoToolsNoInjection(t *testing.T) {
	req := official.APIRequest{
		Model:    "gpt-5",
		Messages: []official.APIMessage{official.NewTextMessage("user", "hi")},
	}
	out := testConvert(t, req)
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	if out.Messages[0].Author.Role != "user" {
		t.Fatalf("role = %q", out.Messages[0].Author.Role)
	}
}

func TestConvertAPIRequestMapsReasoningEffortToWebEnum(t *testing.T) {
	tests := []struct {
		name   string
		effort string
		want   string
	}{
		{name: "default", effort: "", want: "standard"},
		{name: "minimal", effort: "minimal", want: "standard"},
		{name: "low", effort: "low", want: "standard"},
		{name: "medium", effort: "medium", want: "extended"},
		{name: "standard", effort: "standard", want: "standard"},
		{name: "extended", effort: "extended", want: "extended"},
		{name: "high", effort: "high", want: "max"},
		{name: "xhigh", effort: "xhigh", want: "max"},
		{name: "max", effort: "max", want: "max"},
		{name: "unknown", effort: "turbo", want: "standard"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := testConvert(t, official.APIRequest{
				Model:           "gpt-5",
				ReasoningEffort: tt.effort,
				Messages:        []official.APIMessage{official.NewTextMessage("user", "hi")},
			})
			if out.ThinkingEffort != tt.want {
				t.Fatalf("ThinkingEffort = %q, want %q", out.ThinkingEffort, tt.want)
			}
		})
	}
}

func TestConvertAPIRequestInjectsToolInstructions(t *testing.T) {
	req := official.APIRequest{
		Model: "gpt-5",
		Tools: []official.Tool{
			{Type: "function", Function: official.ToolFunction{
				Name:        "bash",
				Description: "Run a shell command",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
			}},
		},
		Messages: []official.APIMessage{
			official.NewTextMessage("user", "list files"),
		},
	}
	out := testConvert(t, req)
	if len(out.Messages) < 2 {
		t.Fatalf("messages = %d, want ≥ 2 (system + user + nudge)", len(out.Messages))
	}
	// 头部应该是 system 消息,包含工具说明
	first := out.Messages[0]
	if first.Author.Role != "system" {
		t.Fatalf("first role = %q, want system", first.Author.Role)
	}
	firstText, _ := first.Content.Parts[0].(string)
	for _, want := range []string{"bash", "Run a shell command", "TOOL CALLING FORMAT", "<tool_call>"} {
		if !strings.Contains(firstText, want) {
			t.Errorf("system message missing %q", want)
		}
	}
}

func TestConvertAPIRequestDoesNotForceFinalNudgeForAutoUserTurn(t *testing.T) {
	req := official.APIRequest{
		Model: "gpt-5",
		Tools: []official.Tool{
			{Type: "function", Function: official.ToolFunction{Name: "bash"}},
		},
		Messages: []official.APIMessage{
			official.NewTextMessage("user", "Working directory: /home/x\nlist"),
		},
	}
	out := testConvert(t, req)
	last := out.Messages[len(out.Messages)-1]
	if last.Author.Role != "user" {
		t.Fatalf("last role = %q, want user", last.Author.Role)
	}
	lastText, _ := last.Content.Parts[0].(string)
	if strings.Contains(lastText, "READ CAREFULLY") {
		t.Fatalf("auto tool choice unexpectedly forced a tool call: %s", lastText)
	}
	if lastText != "Working directory: /home/x\nlist" {
		t.Fatalf("last user message changed unexpectedly: %q", lastText)
	}
}

func TestConvertAPIRequestHandlesToolResult(t *testing.T) {
	req := official.APIRequest{
		Model: "gpt-5",
		Tools: []official.Tool{
			{Type: "function", Function: official.ToolFunction{Name: "bash"}},
		},
		Messages: []official.APIMessage{
			{Role: "assistant", Content: official.MessageContent{TextValue: ""}, ToolCalls: []official.ToolCallRef{{ID: "c1", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "bash", Arguments: `{"command":"ls"}`}}}},
			{Role: "tool", ToolCallID: "c1", Name: "bash", Content: official.MessageContent{TextValue: "file1.py\nfile2.py"}},
		},
	}
	out := testConvert(t, req)
	// ChatGPT Web does not reliably accept author.role="tool". Aurora must
	// convert the OpenAI tool result into an explicit user/context envelope.
	var toolMsg string
	for _, m := range out.Messages {
		if m.Author.Role == "user" && len(m.Content.Parts) > 0 {
			text, _ := m.Content.Parts[0].(string)
			if strings.Contains(text, "[HOST TOOL RESULT]") {
				toolMsg = text
			}
		}
	}
	if !strings.Contains(toolMsg, "Tool: bash") {
		t.Fatalf("tool message missing host-tool prefix: %q", toolMsg)
	}
	if !strings.Contains(toolMsg, "file1.py") {
		t.Fatalf("tool message missing content: %q", toolMsg)
	}
}

func TestConvertAPIRequestSerializesHistoryToolCalls(t *testing.T) {
	req := official.APIRequest{
		Model: "gpt-5",
		Tools: []official.Tool{
			{Type: "function", Function: official.ToolFunction{Name: "bash"}},
		},
		Messages: []official.APIMessage{
			{Role: "user", Content: official.MessageContent{TextValue: "list"}},
			{Role: "assistant", Content: official.MessageContent{TextValue: ""}, ToolCalls: []official.ToolCallRef{{ID: "c1", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "bash", Arguments: `{"command":"ls"}`}}}},
		},
	}
	out := testConvert(t, req)
	// 找到 assistant 消息,确认 <tool_call> 标签已序列化
	var found bool
	for _, m := range out.Messages {
		if m.Author.Role == "assistant" {
			parts := m.Content.Parts
			for _, p := range parts {
				if s, ok := p.(string); ok && strings.Contains(s, "<tool_call>") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("assistant message missing <tool_call> serialization: %#v", out.Messages)
	}
}

func TestConvertAPIRequestForcedToolChoice(t *testing.T) {
	choice := &official.ToolChoice{Type: "function", Function: &official.ToolChoiceFunction{Name: "bash"}}
	req := official.APIRequest{
		Model:      "gpt-5",
		Tools:      []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
		ToolChoice: choice,
		Messages:   []official.APIMessage{official.NewTextMessage("user", "x")},
	}
	out := testConvert(t, req)
	text, _ := out.Messages[0].Content.Parts[0].(string)
	if !strings.Contains(text, `MUST call the tool "bash"`) {
		t.Fatalf("missing forced-call line: %s", text)
	}
}

func TestConvertAPIRequestToolChoiceNoneStripsProtocol(t *testing.T) {
	// tool_choice=none + tools:仍要教模型协议(否则它不知道 "none" 是什么意思),
	// 但要追加 "DISABLED tool calling" 警告
	req := official.APIRequest{
		Model:      "gpt-5",
		Tools:      []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
		ToolChoice: &official.ToolChoice{Type: "none"},
		Messages:   []official.APIMessage{official.NewTextMessage("user", "just answer in text")},
	}
	out := testConvert(t, req)
	text, _ := out.Messages[0].Content.Parts[0].(string)
	if !strings.Contains(text, "DISABLED tool calling") {
		t.Fatalf("missing none-warning: %s", text)
	}
}

type inlineUploadRequest struct {
	method  httpclient.HttpMethod
	url     string
	headers httpclient.AuroraHeaders
	body    []byte
}

type inlineUploadClient struct {
	requests []inlineUploadRequest
}

func (c *inlineUploadClient) Request(method httpclient.HttpMethod, url string, headers httpclient.AuroraHeaders, _ []*http.Cookie, body io.Reader) (*http.Response, error) {
	var data []byte
	if body != nil {
		var err error
		data, err = io.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}
	c.requests = append(c.requests, inlineUploadRequest{method: method, url: url, headers: headers, body: data})

	switch len(c.requests) {
	case 1:
		return testHTTPResponse(http.StatusOK, `{"file_id":"file-uploaded","upload_url":"https://upload.example.test/blob","library_file_id":"library-uploaded"}`), nil
	case 2:
		return testHTTPResponse(http.StatusCreated, ""), nil
	case 3:
		return testHTTPResponse(http.StatusOK, `{}`), nil
	default:
		return nil, fmt.Errorf("unexpected upload request %s %s", method, url)
	}
}

func (c *inlineUploadClient) SetProxy(string) error             { return nil }
func (c *inlineUploadClient) SetCookies(string, []*http.Cookie) {}
func (c *inlineUploadClient) GetCookies(string) []*http.Cookie  { return nil }

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestBuildMessagePartsUploadsOpenCodeFileDataURL(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	payload := `{
		"role":"user",
		"content":[
			{"type":"text","text":"inspect this image"},
			{"type":"file","mime":"image/png","filename":"pixel.png","url":"` + dataURL + `"}
		]
	}`
	var message official.APIMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		t.Fatalf("unmarshal OpenCode message: %v", err)
	}

	client := &inlineUploadClient{}
	account := accounts.NewAccount("test", accounts.TypeFree, "access-token")
	parts, metadata := buildMessageParts(message, client, account, "")

	if len(client.requests) != 3 {
		t.Fatalf("upload request count = %d, want create + put + confirm; Files() = %#v", len(client.requests), message.Files())
	}
	if client.requests[0].method != httpclient.POST || !strings.HasSuffix(client.requests[0].url, "/files") {
		t.Fatalf("create upload request = %s %s", client.requests[0].method, client.requests[0].url)
	}
	var createPayload map[string]interface{}
	if err := json.Unmarshal(client.requests[0].body, &createPayload); err != nil {
		t.Fatalf("decode create upload payload: %v", err)
	}
	if createPayload["file_name"] != "pixel.png" || createPayload["mime_type"] != "image/png" {
		t.Fatalf("create upload payload lost OpenCode attachment metadata: %#v", createPayload)
	}
	if client.requests[1].method != httpclient.PUT || client.requests[1].url != "https://upload.example.test/blob" {
		t.Fatalf("blob upload request = %s %s", client.requests[1].method, client.requests[1].url)
	}
	if len(client.requests[1].body) == 0 || !strings.HasPrefix(string(client.requests[1].body), "\x89PNG\r\n\x1a\n") {
		t.Fatalf("blob upload body is not decoded PNG data: %x", client.requests[1].body)
	}
	if client.requests[1].headers["Content-Type"] != "image/png" {
		t.Fatalf("blob Content-Type = %q, want image/png", client.requests[1].headers["Content-Type"])
	}
	if client.requests[2].method != httpclient.POST || !strings.HasSuffix(client.requests[2].url, "/files/file-uploaded/uploaded") {
		t.Fatalf("confirm upload request = %s %s", client.requests[2].method, client.requests[2].url)
	}

	if len(parts) < 1 {
		t.Fatal("converter returned no content parts")
	}
	imagePart, ok := parts[0].(map[string]interface{})
	if !ok || imagePart["asset_pointer"] != "file-service://file-uploaded" {
		t.Fatalf("image part = %#v, want uploaded file-service pointer", parts[0])
	}
	if metadata == nil {
		t.Fatal("converter dropped attachment metadata")
	}
	attachments, ok := metadata["attachments"].([]interface{})
	if !ok || len(attachments) != 1 {
		t.Fatalf("attachments = %#v, want one uploaded attachment", metadata["attachments"])
	}
	attachment := attachments[0].(map[string]interface{})
	if attachment["id"] != "file-uploaded" || attachment["mime_type"] != "image/png" {
		t.Fatalf("unexpected uploaded attachment: %#v", attachment)
	}
}

func TestBuildMessagePartsUploadsOpenCodeImageURLWireFormat(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	payload := `{
		"role":"user",
		"content":[
			{"type":"text","text":"inspect this image"},
			{"type":"image_url","image_url":{"url":"` + dataURL + `"}}
		]
	}`
	var message official.APIMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		t.Fatalf("unmarshal OpenCode wire message: %v", err)
	}

	client := &inlineUploadClient{}
	account := accounts.NewAccount("test", accounts.TypeFree, "access-token")
	parts, metadata := buildMessageParts(message, client, account, "")

	if len(client.requests) != 3 {
		t.Fatalf("upload request count = %d, want create + put + confirm", len(client.requests))
	}
	var createPayload map[string]interface{}
	if err := json.Unmarshal(client.requests[0].body, &createPayload); err != nil {
		t.Fatalf("decode create upload payload: %v", err)
	}
	if createPayload["file_name"] != "image.png" || createPayload["mime_type"] != "image/png" {
		t.Fatalf("unexpected image_url upload metadata: %#v", createPayload)
	}
	imagePart, ok := parts[0].(map[string]interface{})
	if !ok || imagePart["asset_pointer"] != "file-service://file-uploaded" {
		t.Fatalf("image part = %#v, want uploaded file-service pointer", parts[0])
	}
	attachments, ok := metadata["attachments"].([]interface{})
	if !ok || len(attachments) != 1 {
		t.Fatalf("attachments = %#v, want one uploaded attachment", metadata["attachments"])
	}
}
