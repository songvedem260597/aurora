package handler

import (
	"encoding/json"
	"fmt"
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

func TestRequestToolCallingEnabledHonorsNone(t *testing.T) {
	cfg := &config.Config{ToolCallingEnabled: true}
	req := &officialtypes.APIRequest{
		Tools:      []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		ToolChoice: &officialtypes.ToolChoice{Type: "none"},
	}
	if requestToolCallingEnabled(req, cfg) {
		t.Fatal("tool_choice=none must bypass tool mode even when tools are present")
	}
	req.ToolChoice = &officialtypes.ToolChoice{Type: "auto"}
	if !requestToolCallingEnabled(req, cfg) {
		t.Fatal("tool_choice=auto with tools must keep tool mode enabled")
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

func TestDeferredResponseDetectionDoesNotDependOnActionVerbList(t *testing.T) {
	if !looksLikeDeferredToolAction("Mình sẽ đổi gameplay thành cuộn dọc và thêm nhạc nền.") {
		t.Fatal("generic future commitment should be detected as a deferred response")
	}
	if !looksLikeDeferredToolAction("I will adjust the existing artifact to match that request.") {
		t.Fatal("English future commitment should be detected without classifying the action verb")
	}
	if looksLikeDeferredToolAction("Gameplay hiện tại cuộn ngang vì trục X đang điều khiển tốc độ.") {
		t.Fatal("a direct informational answer must not be treated as deferred work")
	}
}

func TestParseAgentIntentStripsInternalMarker(t *testing.T) {
	intent, clean := parseAgentIntent("<agent_intent>action</agent_intent>Đã cập nhật file.")
	if intent != agentIntentAction {
		t.Fatalf("intent = %q, want action", intent)
	}
	if clean != "Đã cập nhật file." {
		t.Fatalf("clean text = %q", clean)
	}

	intent, clean = parseAgentIntent("<agent_intent>answer</agent_intent>Closure giữ lexical scope.")
	if intent != agentIntentAnswer || clean != "Closure giữ lexical scope." {
		t.Fatalf("answer marker parse = (%q, %q)", intent, clean)
	}
}

func TestAgentIntentRequiresToolWithoutActionKeywords(t *testing.T) {
	messages := []officialtypes.APIMessage{{
		Role:    "user",
		Content: officialtypes.MessageContent{TextValue: "cho nó giống cái bên trái hơn một chút"},
	}}
	if !agentIntentRequiresTool(agentIntentAction, messages) {
		t.Fatal("semantic action intent must require a tool without relying on verb keywords")
	}
	if agentIntentRequiresTool(agentIntentAnswer, messages) {
		t.Fatal("semantic answer intent must remain a normal text turn")
	}

	call := officialtypes.ToolCallRef{ID: "call_edit", Type: "function"}
	call.Function.Name = "edit"
	messages = append(messages, officialtypes.APIMessage{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{call}})
	if agentIntentRequiresTool(agentIntentAction, messages) {
		t.Fatal("an action intent must not force a duplicate tool after one already ran")
	}
}

func TestImplicitContentFollowupInheritsGateAfterModelChoosesTool(t *testing.T) {
	readCall := officialtypes.ToolCallRef{ID: "call_read", Type: "function"}
	readCall.Function.Name = "read"
	readCall.Function.Arguments = `{"filePath":"C:\\Users\\uchih\\Desktop\\pixel-plane-game.html"}`
	messages := []officialtypes.APIMessage{
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "tạo game bắn máy bay pixel bằng html trên desktop"}},
		{Role: "assistant", Content: officialtypes.MessageContent{TextValue: "Đã tạo game."}},
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "bay theo chiều dọc á với có nhạc nữa"}},
		{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{readCall}},
		{Role: "tool", ToolCallID: "call_read", Name: "read", Content: officialtypes.MessageContent{TextValue: "<html>existing game</html>"}},
	}
	if !conversationRequiresContentWork(messages) {
		t.Fatal("once the model semantically chooses a tool for an implicit follow-up, prior content-task context must be inherited")
	}
	req := &officialtypes.APIRequest{
		Tools:    []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "read"}}, {Type: "function", Function: officialtypes.ToolFunction{Name: "apply_patch"}}},
		Messages: messages,
	}
	if !shouldRequireToolCall(req, "Mình sẽ tiếp tục.") {
		t.Fatal("read-only inspection must not complete an implicit code/game modification follow-up")
	}
}

func TestFailedContentMutationDoesNotCountAsCompletedWork(t *testing.T) {
	patchCall := officialtypes.ToolCallRef{ID: "call_patch", Type: "function"}
	patchCall.Function.Name = "apply_patch"
	patchCall.Function.Arguments = `{"patchText":"*** Begin Patch"}`
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "apply_patch"}}, {Type: "function", Function: officialtypes.ToolFunction{Name: "read"}}},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "tạo game html trên desktop"}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{patchCall}},
			{Role: "tool", ToolCallID: "call_patch", Name: "apply_patch", Content: officialtypes.MessageContent{TextValue: "apply_patch verification failed: Failed to find expected lines"}},
		},
	}
	if hasContentMutationToolCallSinceLastUser(req.Messages) {
		t.Fatal("a failed apply_patch must not satisfy the content mutation gate")
	}
	if !latestToolResultFailed(req.Messages) {
		t.Fatal("failed tool result should activate recovery behavior")
	}
	if !shouldRequireToolCall(req, "Đã sửa xong.") {
		t.Fatal("failed mutation must require another real tool action")
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

func TestAttachedImageQuestionDoesNotForceHostToolFromSemanticActionMarker(t *testing.T) {
	// This is the exact OpenCode session wire shape for --file attachments:
	// synthetic Read helper text + a type=file data URL + the real user prompt.
	body := `{"role":"user","content":[{"type":"text","text":"Called the Read tool with the following input"},{"type":"text","text":"Image read successfully"},{"type":"file","mime":"image/jpeg","filename":"photo.jpg","url":"data:image/jpeg;base64,/9j/4AAQ"},{"type":"text","text":"Ảnh này là ảnh gì? Trả lời ngắn gọn."}]}`
	body = strings.ReplaceAll(body, "\\\"", "\"")
	var latest officialtypes.APIMessage
	if err := json.Unmarshal([]byte(body), &latest); err != nil {
		t.Fatalf("decode OpenCode image message: %v", err)
	}

	// An informational image question must not inherit the mutation gate from
	// earlier coding work merely because it shares a long-running session.
	writeCall := officialtypes.ToolCallRef{ID: "call_write", Type: "function"}
	writeCall.Function.Name = "apply_patch"
	writeCall.Function.Arguments = `{"patchText":"*** Begin Patch"}`
	messages := []officialtypes.APIMessage{
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "tạo game html"}},
		{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{writeCall}},
		{Role: "tool", ToolCallID: "call_write", Content: officialtypes.MessageContent{TextValue: "Done!"}},
		latest,
	}

	if !latestUserHasAttachment(messages) {
		t.Fatal("OpenCode image_url turn should be recognized as containing an attachment")
	}
	if userExplicitlyRequestsTool(messages) {
		t.Fatal("synthetic OpenCode attachment helper text must not count as an explicit tool request")
	}
	if conversationRequestsAction(messages) {
		t.Fatal("an informational image question must not count as a host action request")
	}
	if conversationRequestsMutation(messages) {
		t.Fatal("an informational image question must not count as a workspace mutation")
	}
	if conversationRequiresContentWork(messages) {
		t.Fatal("an informational image question must not count as coding/content work")
	}
	if agentIntentRequiresTool(agentIntentAction, messages) {
		t.Fatal("an already-attached image question must not inherit a mandatory host tool requirement")
	}
	req := &officialtypes.APIRequest{
		Tools:    []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: messages,
	}
	if shouldRequireToolCall(req, "Đây là ảnh một chiếc váy trên mannequin.") {
		t.Fatal("an attached image question with a normal answer must not trigger the pre-semantic tool gate")
	}
}

func TestInformationalAttachmentStripsToolProtocolFromUpstreamRequest(t *testing.T) {
	parallel := true
	choice := &officialtypes.ToolChoice{Type: "auto"}
	original := &officialtypes.APIRequest{
		Tools:             []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		ToolChoice:        choice,
		ParallelToolCalls: &parallel,
		Messages: []officialtypes.APIMessage{{
			Role: "user",
			Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
				{Type: "text", Text: "ảnh gì đây"},
				{Type: "file", URL: "data:image/jpeg;base64,/9j/4AAQ", Mime: "image/jpeg", FileName: `C:\\Users\\uchih\\Downloads\\photo.jpg`},
			}},
		}},
	}

	prepared, informational := toolUpstreamRequest(original)
	if !informational {
		t.Fatal("direct OpenCode image question should use informational attachment mode")
	}
	if len(prepared.Tools) != 0 || prepared.ToolChoice != nil || prepared.ParallelToolCalls != nil {
		t.Fatalf("tool protocol leaked into informational vision request: %#v", prepared)
	}
	if len(original.Tools) != 1 || original.ToolChoice != choice || original.ParallelToolCalls == nil {
		t.Fatal("preparing the upstream request must not mutate the original OpenCode request")
	}
}

func TestToolUpstreamRequestCompactsLongHistory(t *testing.T) {
	messages := []officialtypes.APIMessage{
		officialtypes.NewTextMessage("system", "keep this system instruction"),
	}
	for i := 0; i < 80; i++ {
		role := "assistant"
		if i%4 == 0 {
			role = "user"
		}
		messages = append(messages, officialtypes.NewTextMessage(role, fmt.Sprintf("history-%d", i)))
	}
	original := &officialtypes.APIRequest{
		Tools:    []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: messages,
	}

	prepared, informational := toolUpstreamRequest(original)
	if informational {
		t.Fatal("text-only request unexpectedly entered informational attachment mode")
	}
	if len(prepared.Messages) >= len(messages) || len(prepared.Messages) > 33 {
		t.Fatalf("history was not bounded: got %d messages from %d", len(prepared.Messages), len(messages))
	}
	if prepared.Messages[0].Role != "system" || prepared.Messages[0].Text() != "keep this system instruction" {
		t.Fatalf("system instruction was not preserved: %#v", prepared.Messages[0])
	}
	if prepared.Messages[len(prepared.Messages)-1].Text() != "history-79" {
		t.Fatal("latest message was dropped during compaction")
	}
	if len(original.Messages) != len(messages) {
		t.Fatal("compaction mutated the original request")
	}
}

func TestInformationalAttachmentUsesSmallRecentWindow(t *testing.T) {
	messages := []officialtypes.APIMessage{officialtypes.NewTextMessage("system", "system")}
	for i := 0; i < 40; i++ {
		messages = append(messages, officialtypes.NewTextMessage("user", fmt.Sprintf("old-%d", i)))
		messages = append(messages, officialtypes.NewTextMessage("assistant", fmt.Sprintf("answer-%d", i)))
	}
	messages = append(messages, officialtypes.APIMessage{
		Role: "user",
		Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
			{Type: "text", Text: "đây là gì"},
			{Type: "image_url", ImageURL: &officialtypes.ImageURLDetail{URL: "data:image/jpeg;base64,/9j/4AAQ"}},
		}},
	})
	original := &officialtypes.APIRequest{
		Tools:    []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: messages,
	}

	prepared, informational := toolUpstreamRequest(original)
	if !informational {
		t.Fatal("image question did not enter informational attachment mode")
	}
	if len(prepared.Messages) > 9 {
		t.Fatalf("informational image history is still too large: %d messages", len(prepared.Messages))
	}
	if len(prepared.Messages[len(prepared.Messages)-1].Files()) != 1 {
		t.Fatal("latest image attachment was dropped")
	}
}

func TestPrepareDirectInformationalAttachmentPreservesStreamingImage(t *testing.T) {
	stream := true
	parallel := true
	request := &officialtypes.APIRequest{
		Stream:            stream,
		ParallelToolCalls: &parallel,
		ToolChoice:        &officialtypes.ToolChoice{Type: "auto"},
		Tools:             []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{
			officialtypes.NewTextMessage("system", "system"),
			officialtypes.NewTextMessage("user", "older context"),
			officialtypes.NewTextMessage("assistant", "older answer"),
			{
				Role: "user",
				Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
					{Type: "text", Text: "mô tả ảnh này"},
					{Type: "image_url", ImageURL: &officialtypes.ImageURLDetail{URL: "data:image/png;base64,iVBORw0"}},
				}},
			},
		},
	}

	if !prepareDirectInformationalAttachment(request) {
		t.Fatal("plain image description was not routed to direct streaming")
	}
	if !request.Stream {
		t.Fatal("direct image answer lost the client's stream request")
	}
	if len(request.Tools) != 0 || request.ToolChoice != nil || request.ParallelToolCalls != nil {
		t.Fatalf("host tool protocol was not removed: %#v", request)
	}
	if len(request.Messages) != 1 || request.Messages[0].Role != "user" {
		t.Fatalf("direct image answer retained unrelated history: %#v", request.Messages)
	}
	if len(request.Messages[len(request.Messages)-1].Files()) != 1 {
		t.Fatal("direct streaming request dropped the attached image")
	}
}

func TestPrepareDirectInformationalAttachmentAcceptsOpenCodeReadHelperText(t *testing.T) {
	request := &officialtypes.APIRequest{
		Stream: true,
		Messages: []officialtypes.APIMessage{{
			Role: "user",
			Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
				{Type: "text", Text: `Called the Read tool with the following input: {"filePath":"C:\\Users\\test\\outfit.jpg"}`},
				{Type: "text", Text: "Image read successfully"},
				{Type: "image_url", ImageURL: &officialtypes.ImageURLDetail{URL: "data:image/jpeg;base64,/9j/4AAQ"}},
				{Type: "text", Text: "Test lần 3: mô tả trang phục trong ảnh bằng một câu."},
			}},
		}},
	}

	if !prepareDirectInformationalAttachment(request) {
		t.Fatal("OpenCode's synthetic Read helper text incorrectly disabled direct vision")
	}
	if len(request.Messages) != 1 || len(request.Messages[0].Files()) != 1 {
		t.Fatalf("current OpenCode image turn was not preserved: %#v", request.Messages)
	}
}

func TestAttachmentMutationKeepsToolProtocolInUpstreamRequest(t *testing.T) {
	original := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{{
			Role: "user",
			Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
				{Type: "image_url", ImageURL: &officialtypes.ImageURLDetail{URL: "data:image/png;base64,iVBORw0KGgo="}},
				{Type: "text", Text: "Sửa giao diện trong workspace giống screenshot này."},
			}},
		}},
	}

	prepared, informational := toolUpstreamRequest(original)
	if informational {
		t.Fatal("a screenshot-based workspace edit must not enter answer-only vision mode")
	}
	if len(prepared.Tools) != 1 || prepared.Tools[0].Function.Name != "bash" {
		t.Fatalf("workspace mutation lost its host tools: %#v", prepared.Tools)
	}
}

func TestAttachedImageMutationStillRequiresHostTool(t *testing.T) {
	messages := []officialtypes.APIMessage{{
		Role: "user",
		Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
			{Type: "image_url", ImageURL: &officialtypes.ImageURLDetail{URL: "data:image/png;base64,iVBORw0KGgo="}},
			{Type: "text", Text: "Edit this image and save the result in the workspace."},
		}},
	}}

	if !agentIntentRequiresTool(agentIntentAction, messages) {
		t.Fatal("an attachment must not bypass tools when the user actually requests a workspace mutation")
	}
}

func TestAttachedImageQuestionDoesNotEscalateSandboxStyleTextToHostTool(t *testing.T) {
	req := &officialtypes.APIRequest{
		Tools: []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		Messages: []officialtypes.APIMessage{{
			Role: "user",
			Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
				{Type: "text", Text: "Image read successfully"},
				{Type: "image_url", ImageURL: &officialtypes.ImageURLDetail{URL: "data:image/jpeg;base64,/9j/4AAQ"}},
				{Type: "text", Text: "Ảnh này là ảnh gì?"},
			}},
		}},
	}

	if shouldRequireToolCall(req, "I cannot inspect /mnt/data from this environment.") {
		t.Fatal("an informational attachment turn must not convert response text into a mandatory host tool call")
	}
}

func TestAttachmentAccessRefusalDetection(t *testing.T) {
	for _, text := range []string{
		"Mình chưa thể nhìn trực tiếp ảnh này vì hệ thống báo tệp không được hỗ trợ cho đầu vào hình ảnh.",
		"Mình không xem được ảnh này.",
		"Image input is not supported here.",
		"I cannot access the image in this conversation.",
	} {
		if !looksLikeAttachmentAccessRefusal(text) {
			t.Fatalf("expected attachment refusal to be detected: %q", text)
		}
	}
	for _, text := range []string{
		"Đây là mannequin mặc váy đỏ phong cách gothic.",
		"Ảnh hơi mờ nhưng vẫn thấy một người đứng trước gương.",
	} {
		if looksLikeAttachmentAccessRefusal(text) {
			t.Fatalf("normal vision answer must not be treated as refusal: %q", text)
		}
	}
}

func TestVisionAnswerRetryIncludesMarkerOnlyResponse(t *testing.T) {
	messages := []officialtypes.APIMessage{{
		Role: "user",
		Content: officialtypes.MessageContent{Parts: []officialtypes.MessageContentPart{
			{Type: "image_url", ImageURL: &officialtypes.ImageURLDetail{URL: "data:image/jpeg;base64,/9j/4AAQ"}},
			{Type: "text", Text: "Ảnh này là ảnh gì?"},
		}},
	}}

	for _, text := range []string{
		"",
		"Mình không xem được ảnh này.",
	} {
		if !shouldRetryVisionAnswer(messages, text) {
			t.Fatalf("expected vision retry for %q", text)
		}
	}
	if shouldRetryVisionAnswer(messages, "Đây là mannequin mặc váy đỏ.") {
		t.Fatal("normal vision answer must not retry")
	}
	if shouldRetryVisionAnswer([]officialtypes.APIMessage{{
		Role:    "user",
		Content: officialtypes.MessageContent{TextValue: "Giải thích closure"},
	}}, "") {
		t.Fatal("an empty text-only answer must keep the ordinary completion retry path")
	}
}

func TestContentTaskExtensionMatchingRequiresFilenameBoundary(t *testing.T) {
	imageHelper := `Called the Read tool with the following input: {"filePath":"C:\\Users\\uchih\\Downloads\\_any.clothes_1756508549.jpg"}`
	if textLooksLikeContentTask(normalizeIntentText(imageHelper)) {
		t.Fatal(".clothes in an image filename must not be mistaken for the .c source extension")
	}
	for _, sourcePath := range []string{"main.c", "src/app.tsx", `C:\\work\\server.go:42`} {
		if !textLooksLikeContentTask(normalizeIntentText(sourcePath)) {
			t.Fatalf("real source path %q should still be classified as content work", sourcePath)
		}
	}
}

func TestDeferredResponseRequiresToolForImplicitContentFollowup(t *testing.T) {
	messages := []officialtypes.APIMessage{
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "tạo game bắn máy bay pixel bằng html trên desktop"}},
		{Role: "assistant", Content: officialtypes.MessageContent{TextValue: "Đã tạo game."}},
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "bay theo chiều dọc á với có nhạc nữa"}},
	}

	if !deferredResponseRequiresTool(messages) {
		t.Fatal("implicit follow-up to an earlier game task must require a real tool turn")
	}
}

func TestDeferredResponseRequiresToolInheritsSuccessfulMutation(t *testing.T) {
	writeCall := officialtypes.ToolCallRef{ID: "call_write", Type: "function"}
	writeCall.Function.Name = "apply_patch"
	writeCall.Function.Arguments = `{"patchText":"*** Begin Patch"}`
	messages := []officialtypes.APIMessage{
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "làm cái đó đi"}},
		{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{writeCall}},
		{Role: "tool", ToolCallID: "call_write", Content: officialtypes.MessageContent{TextValue: "Done!"}},
		{Role: "assistant", Content: officialtypes.MessageContent{TextValue: "Đã cập nhật."}},
		{Role: "user", Content: officialtypes.MessageContent{TextValue: "cho nhanh hơn chút nữa"}},
	}

	if !deferredResponseRequiresTool(messages) {
		t.Fatal("a terse follow-up must inherit a prior successful workspace mutation")
	}
}

func TestDeferredResponseRequiresToolAllowsPureExplanation(t *testing.T) {
	messages := []officialtypes.APIMessage{{
		Role:    "user",
		Content: officialtypes.MessageContent{TextValue: "giải thích closure là gì"},
	}}

	if deferredResponseRequiresTool(messages) {
		t.Fatal("pure explanation must not be escalated into a mandatory tool turn")
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

func TestNegativeMutationConstraintDoesNotForceExtraTool(t *testing.T) {
	call := officialtypes.ToolCallRef{ID: "call_pwd", Type: "function"}
	call.Function.Name = "bash"
	call.Function.Arguments = `{"command":"pwd"}`
	req := &officialtypes.APIRequest{
		Tools:      []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "bash"}}},
		ToolChoice: &officialtypes.ToolChoice{Type: "auto"},
		Messages: []officialtypes.APIMessage{
			{Role: "user", Content: officialtypes.MessageContent{TextValue: "You must use the bash tool to run pwd. Report its output and do not modify any files."}},
			{Role: "assistant", ToolCalls: []officialtypes.ToolCallRef{call}},
			{Role: "tool", ToolCallID: "call_pwd", Name: "bash", Content: officialtypes.MessageContent{TextValue: `C:\work`}},
		},
	}
	if conversationRequestsMutation(req.Messages) {
		t.Fatal("a negated mutation constraint must not be classified as a mutation request")
	}
	if shouldRequireToolCall(req, "") {
		t.Fatal("a completed pwd call must allow the final answer")
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

	var openCodeReq officialtypes.APIRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5-6",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"What color is this image?"},
			{"type":"file","mime":"image/png","url":"data:image/png;base64,AA==","filename":"aurora_vision_test.png"}
		]}]
	}`), &openCodeReq); err != nil {
		t.Fatalf("unmarshal OpenCode file request: %v", err)
	}
	if !original_requestHasFiles(openCodeReq) {
		t.Fatal("OpenCode type=file+url attachment must require a file-capable account")
	}
}

func TestAttachmentQuotaErrorClassificationAndEnvelope(t *testing.T) {
	if !isAttachmentQuotaError("You've hit your attachment limit. Please try again later.") {
		t.Fatal("expected ChatGPT attachment-limit message to be classified")
	}
	if isAttachmentQuotaError("the model returned an empty response") {
		t.Fatal("unrelated upstream error was classified as attachment quota")
	}

	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	respondAttachmentQuotaError(c)
	if writer.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", writer.Code)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if envelope.Error.Message == "" || envelope.Error.Type != "attachment_limit_error" || envelope.Error.Code != "attachment_limit" {
		t.Fatalf("unexpected error envelope: %#v", envelope.Error)
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

func TestWriteToolCallingSSEMatches9RouterStreamingShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	calls := []officialtypes.ToolCall{{
		Index: 0,
		ID:    "call_test",
		Type:  "function",
		Function: officialtypes.ToolCallFunc{
			Name:      "bash",
			Arguments: `{"command":"pwd"}`,
		},
	}}

	usage := &officialtypes.StreamUsage{
		PromptTokens:     12,
		CompletionTokens: 3,
		TotalTokens:      15,
	}
	writeToolCallingSSE(c, "", calls, "gpt-test", "conv-test", false, usage)
	lines := sseDataLines(writer.Body.String())
	if len(lines) != 4 {
		t.Fatalf("data line count = %d, want role + tool_calls + stop + DONE; output: %s", len(lines), writer.Body.String())
	}

	var toolChunk map[string]interface{}
	if err := json.Unmarshal([]byte(lines[1]), &toolChunk); err != nil {
		t.Fatalf("invalid tool chunk: %v", err)
	}
	toolDelta := toolChunk["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	toolCall := toolDelta["tool_calls"].([]interface{})[0].(map[string]interface{})
	if toolCall["id"] != "call_test" || toolCall["type"] != "function" {
		t.Fatalf("unexpected tool identity chunk: %#v", toolCall)
	}
	fn := toolCall["function"].(map[string]interface{})
	if fn["name"] != "bash" || fn["arguments"] != `{"command":"pwd"}` {
		t.Fatalf("unexpected function chunk: %#v", fn)
	}

	var stopChunk map[string]interface{}
	if err := json.Unmarshal([]byte(lines[2]), &stopChunk); err != nil {
		t.Fatalf("invalid stop chunk: %v", err)
	}
	finish := stopChunk["choices"].([]interface{})[0].(map[string]interface{})["finish_reason"]
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", finish)
	}
	stopUsage, ok := stopChunk["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage = %#v, want usage object on final tool finish chunk", stopChunk["usage"])
	}
	if stopUsage["prompt_tokens"] != float64(12) || stopUsage["completion_tokens"] != float64(3) || stopUsage["total_tokens"] != float64(15) {
		t.Fatalf("unexpected usage: %#v", stopUsage)
	}
	if lines[3] != "[DONE]" {
		t.Fatalf("last data line = %q, want [DONE]", lines[3])
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
