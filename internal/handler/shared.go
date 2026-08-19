package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	chatgpt_types "aurora/typings/chatgpt"
	officialtypes "aurora/typings/official"
	"aurora/util"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

var ErrNoAvailable = errors.New("no available account of the requested type")

func respondError(c *gin.Context, status int, err error) {
	c.JSON(status, gin.H{"error": gin.H{
		"message": err.Error(),
		"type":    "invalid_request_error",
		"param":   nil,
		"code":    http.StatusText(status),
	}})
}

func respondRequestConversionError(c *gin.Context, err error) {
	c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
		"message": err.Error(),
		"type":    "attachment_upload_error",
		"param":   "messages",
		"code":    "attachment_upload_failed",
	}})
}

func isAttachmentQuotaError(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(normalized, "attachment limit") ||
		strings.Contains(normalized, "attachment quota")
}

func respondAttachmentQuotaError(c *gin.Context) {
	// 422 is deliberate: OpenCode retries transient 5xx responses, which turns
	// an exhausted attachment quota into an endless Thinking state.
	c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{
		"message": "all available ChatGPT accounts have exhausted their attachment quota; try again later",
		"type":    "attachment_limit_error",
		"param":   "messages",
		"code":    "attachment_limit",
	}})
}

// resolveAccount 从请求 Authorization header 解析账号
// 替代旧的 secretFromAuthorization + accessTokenFromRefreshToken
// 返回 (account, http_status, error)
func resolveAccount(c *gin.Context, pool *accounts.Pool, cfg *config.Config, needsPaid bool) (*accounts.Account, int, error) {
	authHeader := c.GetHeader("Authorization")

	// 提取 Bearer token
	payload := strings.TrimSpace(authHeader)
	if len(payload) >= 7 && strings.EqualFold(payload[:7], "Bearer ") {
		payload = strings.TrimSpace(payload[7:])
	}
	parts := strings.SplitN(payload, ",", 2)
	token := strings.TrimSpace(parts[0])
	teamAccountID := ""
	if len(parts) > 1 {
		teamAccountID = strings.TrimSpace(parts[1])
	}

	// 补充检查专用 header: ChatGPT-Account-ID, Team-Account-ID 等
	for _, header := range []string{"ChatGPT-Account-ID", "Chatgpt-Account-Id", "Team-Account-ID", "X-ChatGPT-Account-ID"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			teamAccountID = value
			break
		}
	}

	expected := cfg.Authorization

	// 无 token 或匹配全局密钥 → 先尝试 free,再 fallback 到 noauth
	if token == "" || (expected != "" && token == expected) {
		var acct *accounts.Account
		var err error
		if needsPaid {
			acct, err = pool.AcquireForAttachments(accounts.TypeFree)
		} else {
			acct, err = pool.Acquire(accounts.TypeFree)
		}
		if errors.Is(err, accounts.ErrAttachmentLimited) {
			return nil, http.StatusUnprocessableEntity, accounts.ErrAttachmentLimited
		}
		if err != nil || acct == nil {
			// free 池空时(无 session/access/refresh token 账号),fallback 到 noauth(UUID 设备)
			acct, err = pool.Acquire(accounts.TypeNoAuth)
		}
		if err != nil || acct == nil {
			return nil, http.StatusUnauthorized, ErrNoAvailable
		}
		if needsPaid && acct.Type == accounts.TypeNoAuth {
			return nil, http.StatusForbidden, errors.New("this endpoint requires a logged-in ChatGPT account")
		}
		return acct, http.StatusOK, nil
	}

	// access_token (JWT) → 创建/复用临时账号 (受 ENABLE_EXTERNAL_TOKEN 控制)
	if strings.HasPrefix(token, "eyJ") {
		if !cfg.EnableExternalToken {
			return nil, http.StatusUnauthorized, errors.New("external access token disabled (set ENABLE_EXTERNAL_TOKEN=true)")
		}
		userAgent := c.GetHeader("User-Agent")
		proxyURL := cfg.ProxyURL
		if proxyURL == "" {
			proxyURL = cfg.HTTPProxy
		}
		acct := pool.GetOrCreateTempAccount(token, userAgent, proxyURL)
		acct.TeamUserID = teamAccountID
		return acct, http.StatusOK, nil
	}

	// UUID → noauth 账号
	if _, err := uuid.Parse(token); err == nil {
		if needsPaid {
			return nil, http.StatusForbidden, errors.New("this endpoint requires a paid ChatGPT account")
		}
		acct := accounts.NewAccount(token, accounts.TypeNoAuth, token)
		if err := acct.InitClient(); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		acct.Status = accounts.StatusActive
		return acct, http.StatusOK, nil
	}

	// refresh_token → 换 access_token
	if teamAccountID != "" || len(token) > 64 {
		client := bogdanfinn.NewStdClient()
		result, status, err := chatgpt.GETTokenForRefreshToken(client, token, cfg.ProxyURL)
		if err != nil {
			return nil, status, err
		}
		if data, ok := result.(map[string]interface{}); ok {
			if accessToken, ok := data["access_token"].(string); ok && accessToken != "" {
				acct := accounts.NewAccount(accessToken, accounts.TypeFree, accessToken)
				acct.TeamUserID = teamAccountID
				acct.Proxy = cfg.ProxyURL
				acct.RefreshToken = token
				if err := acct.InitClient(); err != nil {
					return nil, http.StatusInternalServerError, err
				}
				acct.Status = accounts.StatusActive
				return acct, http.StatusOK, nil
			}
		}
		return nil, http.StatusBadRequest, errors.New("refresh token response did not include access_token")
	}

	// 兜底：从池里取
	var acct *accounts.Account
	var err error
	if needsPaid {
		acct, err = pool.AcquireForAttachments(accounts.TypeFree)
	} else {
		acct, err = pool.Acquire(accounts.TypeFree)
	}
	if errors.Is(err, accounts.ErrAttachmentLimited) {
		return nil, http.StatusUnprocessableEntity, accounts.ErrAttachmentLimited
	}
	if err != nil {
		return nil, http.StatusUnauthorized, ErrNoAvailable
	}
	if needsPaid && acct.Type == accounts.TypeNoAuth {
		return nil, http.StatusForbidden, errors.New("this endpoint requires a logged-in ChatGPT account")
	}
	acct.LastUsed = time.Now()
	return acct, http.StatusOK, nil
}

// conversationClientOrder 执行标准的 conversation 流程：
// sentinel → init → ws → prepare → POST
//
// 对齐 initialize/handlers.go:postConversationGptClientOrder
// pool 参数用于在 sentinel 401 时标记账号不可用
func conversationClientOrder(client **bogdanfinn.TlsClient, account *accounts.Account, translatedRequest chatgpt_types.ChatGPTRequest, proxyUrl string, stream bool, state *chatgpt.ChatClientState, pool *accounts.Pool) (*http.Response, *websocket.Conn, *chatgpt.TurnStile, int, error) {
	if state != nil {
		state.ApplyToRequest(&translatedRequest)
	}
	turnTraceID := uuid.NewString()

	(*client).SetCookies("https://chatgpt.com", chatgpt.BasicCookies)

	turnStile, status, err := chatgpt.InitSentinelWithState(*client, account, proxyUrl, 0, state)
	if err != nil {
		// sentinel 401 说明 token 可能过期，标记账号让 pool 后续绕过
		if status == http.StatusUnauthorized && pool != nil {
			pool.ReportFailure(account)
		}
		return nil, nil, nil, status, err
	}

	chatgpt.POSTConversationInit(*client, account, state)

	var wsConn *websocket.Conn
	if chatgpt.RequiresConversationWebsocket(stream, translatedRequest.ThinkingEffort) && account.Type.Satisfies(accounts.CapWebSocket) {
		wsConn, err = chatgpt.DialChatWebsocketWithStateAndProxy(*client, account, state, proxyUrl)
		if err != nil {
			return nil, nil, nil, http.StatusInternalServerError, err
		}
	}

	conduitToken, err := chatgpt.PrepareConversationConduitFullWithSentinel(*client, translatedRequest, account, proxyUrl, turnTraceID, state, turnStile)
	if err != nil {
		if wsConn != nil {
			wsConn.Close()
		}
		return nil, nil, nil, http.StatusInternalServerError, err
	}

	response, err := chatgpt.POSTconversationPreparedWithState(*client, translatedRequest, account, turnStile, proxyUrl, conduitToken, turnTraceID, state)
	if err != nil {
		if wsConn != nil {
			wsConn.Close()
		}
		return nil, nil, nil, http.StatusInternalServerError, err
	}
	return response, wsConn, turnStile, http.StatusOK, nil
}

// setupClientWithProxy 创建带代理的 std client
func setupClientWithProxy(proxyUrl string) *bogdanfinn.TlsClient {
	client := bogdanfinn.NewStdClient()
	if proxyUrl != "" {
		_ = client.SetProxy(proxyUrl)
	}
	return client
}

// websocketProxyFunc 为 WebSocket 连接配置代理（从原 request.go 复制）
func websocketProxyFunc(proxy string) (func(*fhttp.Request) (*url.URL, error), error) {
	if proxy == "" {
		return fhttp.ProxyFromEnvironment, nil
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}
	return fhttp.ProxyURL(proxyURL), nil
}

// original_requestHasFiles 检查请求消息中是否包含文件引用
func original_requestHasFiles(request officialtypes.APIRequest) bool {
	for _, message := range request.Messages {
		if len(message.Files()) > 0 {
			return true
		}
	}
	return false
}

// toolCallingEnabled 根据 Config + Tools 列表判定是否启用工具调用模拟。
func toolCallingEnabled(tools []officialtypes.Tool, cfg *config.Config) bool {
	if cfg != nil && !cfg.ToolCallingEnabled {
		return false
	}
	return len(tools) > 0
}

// requestToolCallingEnabled applies the request-level OpenAI/9Router contract.
// OpenCode can keep sending tool definitions while explicitly selecting
// tool_choice="none"; that turn must bypass the tool emulation path entirely.
func requestToolCallingEnabled(request *officialtypes.APIRequest, cfg *config.Config) bool {
	if request == nil || (request.ToolChoice != nil && request.ToolChoice.IsForcedNone()) {
		return false
	}
	return toolCallingEnabled(request.Tools, cfg)
}

// countMessagesTokens 统计消息的 token 数
func countMessagesTokens(messages []officialtypes.APIMessage) int {
	total := 0
	for _, message := range messages {
		total += util.CountToken(message.Text())
	}
	return total
}

// writeChatCompletionStreamDone 写入流式结束标记
func writeChatCompletionStreamDone(c *gin.Context, stopSent bool, model string, conversationID string) {
	if !stopSent {
		finalLine := officialtypes.StopChunkWithConversation("stop", model, conversationID)
		c.Writer.WriteString("data: " + finalLine.String() + "\n\n")
		c.Writer.Flush()
	}
	c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}

// looksLikeSandboxRefusal 检测模型是否声称自己处于隔离环境/无法访问工具。
func looksLikeSandboxRefusal(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	markers := []string{
		"/mnt/data", "/workspace", "/home/oai", "filesystem isolado", "ambiente isolado",
		"root linux", "linux/container", "container atual", "não tem acesso ao diret",
		"nao tem acesso ao diret", "não está montado", "nao esta montado",
		"não foi montado", "nao foi montado", "não existe neste ambiente",
		"nao existe neste ambiente", "não pode continuar neste ambiente",
		"não é possível ler", "nao e possivel ler",
		"não foi possível abrir", "nao foi possivel abrir",
		"não foi possível executar", "nao foi possivel executar",
		"falha na interface de execução", "falha no parsing",
		"inferência baseada na estrutura", "inferencia baseada na estrutura",
		"baseada apenas na estrutura",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// shouldRequireToolCall decides whether a plain-text response is actually a
// failed tool-call attempt. OpenCode normally sends tool_choice=auto, so merely
// having tools available is not enough: ordinary questions must still be able
// to finish in text. We only force a retry when the client explicitly requires
// a tool, the user explicitly asks for one, or the model says it is about to
// inspect/run/edit the workspace without emitting a tool call.
func shouldRequireToolCall(request *officialtypes.APIRequest, text string) bool {
	if request == nil || len(request.Tools) == 0 {
		return false
	}
	if request.ToolChoice != nil {
		if request.ToolChoice.IsForcedNone() {
			return false
		}
		if request.ToolChoice.RequiresCall() {
			return true
		}
	}
	actionTask := userExplicitlyRequestsTool(request.Messages) || conversationRequestsAction(request.Messages)
	mutationTask := conversationRequestsMutation(request.Messages)
	contentTask := conversationRequiresContentWork(request.Messages)
	if contentTask {
		if !hasContentMutationToolCallSinceLastUser(request.Messages) {
			return true
		}
		if !hasVerificationAfterContentMutation(request.Messages) {
			return true
		}
	} else if mutationTask && !hasMutationToolCallSinceLastUser(request.Messages) {
		return true
	}
	if actionTask && !hasToolCallSinceLastUser(request.Messages) {
		return true
	}
	// An inline attachment is already available to the upstream model. For an
	// informational attachment turn, response wording such as a sandbox/path
	// refusal must not be reinterpreted as a request to run a host tool. The
	// semantic retry can ask the model to answer from the attachment instead.
	if latestUserAttachmentAllowsTextAnswer(request.Messages) {
		return false
	}
	return looksLikeSandboxRefusal(text)
}

func normalizeIntentText(s string) string {
	s = strings.ToLower(norm.NFD.String(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if r == 'đ' {
			r = 'd'
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

const (
	agentIntentAction = "action"
	agentIntentAnswer = "answer"
)

func parseAgentIntent(text string) (string, string) {
	const actionTag = "<agent_intent>action</agent_intent>"
	const answerTag = "<agent_intent>answer</agent_intent>"
	intent := ""
	if strings.Contains(text, actionTag) {
		intent = agentIntentAction
	} else if strings.Contains(text, answerTag) {
		intent = agentIntentAnswer
	}
	clean := strings.ReplaceAll(text, actionTag, "")
	clean = strings.ReplaceAll(clean, answerTag, "")
	return intent, strings.TrimSpace(clean)
}

// agentIntentRequiresTool is deliberately semantic rather than keyword based:
// the upstream model classifies the latest turn as action vs. answer, while
// the server only verifies the structural fact that no tool has run yet.
func agentIntentRequiresTool(intent string, messages []officialtypes.APIMessage) bool {
	if intent != agentIntentAction || hasToolCallSinceLastUser(messages) {
		return false
	}
	// OpenCode represents an uploaded image/file as synthetic helper text plus
	// an inline file part in the same user turn. The helper text can make an
	// upstream semantic classifier think "Read tool" means the user requested
	// host execution, even though the attachment is already available to the
	// model. Treat attachment-only inspection as an answer task unless the
	// actual turn independently carries a host action/mutation/content signal.
	if latestUserAttachmentAllowsTextAnswer(messages) {
		return false
	}
	return true
}

func latestUserAttachmentAllowsTextAnswer(messages []officialtypes.APIMessage) bool {
	return latestUserHasAttachment(messages) &&
		!userExplicitlyRequestsTool(messages) &&
		!conversationRequestsAction(messages) &&
		!conversationRequestsMutation(messages) &&
		!conversationRequiresContentWork(messages)
}

// toolUpstreamRequest removes the emulated host-tool protocol for a turn that
// only asks the model to inspect an already-attached image. OpenCode includes
// its tool definitions on ordinary answer turns too; forwarding the large tool
// prompt for a pure vision question can make the upstream model emit only an
// intent marker or claim the attachment is unavailable. Requests that ask to
// edit code/files based on an image still retain their tools.
func toolUpstreamRequest(request *officialtypes.APIRequest) (officialtypes.APIRequest, bool) {
	if request == nil {
		return officialtypes.APIRequest{}, false
	}
	prepared := *request
	prepared.Messages = compactUpstreamHistory(request.Messages, 32)
	if !latestUserAttachmentAllowsTextAnswer(request.Messages) {
		return prepared, false
	}
	prepared.Tools = nil
	prepared.ToolChoice = nil
	prepared.ParallelToolCalls = nil
	prepared.Messages = compactUpstreamHistory(request.Messages, 8)
	return prepared, true
}

// compactUpstreamHistory bounds the transcript replayed to ChatGPT Web. The
// original request remains available to Aurora's semantic/tool gates, while
// upstream only receives recent context. This matters for OpenCode because it
// resends the complete tool transcript on every request; very long sessions
// otherwise spend a minute replaying hundreds of old tool messages.
func compactUpstreamHistory(messages []officialtypes.APIMessage, maxRecent int) []officialtypes.APIMessage {
	if maxRecent <= 0 || len(messages) <= maxRecent {
		return append([]officialtypes.APIMessage(nil), messages...)
	}

	start := len(messages) - maxRecent
	// Avoid beginning with a detached tool result when a nearby user boundary
	// exists inside the retained window.
	for i := start; i < len(messages); i++ {
		if messages[i].Role == "user" {
			start = i
			break
		}
	}

	result := make([]officialtypes.APIMessage, 0, len(messages)-start+2)
	for i := 0; i < start; i++ {
		if messages[i].Role == "system" {
			result = append(result, messages[i])
		}
	}
	result = append(result, messages[start:]...)
	return result
}

func latestUserHasAttachment(messages []officialtypes.APIMessage) bool {
	i := lastUserIndex(messages)
	if i < 0 {
		return false
	}
	return len(messages[i].Files()) > 0
}

func looksLikeAttachmentAccessRefusal(text string) bool {
	t := normalizeIntentText(text)
	markers := []string{
		"khong xem duoc anh",
		"khong the xem anh",
		"khong the nhin anh",
		"chua the nhin truc tiep anh",
		"khong the nhin truc tiep anh",
		"tep khong duoc ho tro",
		"file khong duoc ho tro",
		"khong ho tro dau vao hinh anh",
		"khong duoc ho tro cho dau vao hinh anh",
		"cannot see the image",
		"can't see the image",
		"cannot view the image",
		"cannot access the image",
		"image input is not supported",
		"unsupported image input",
		"unsupported image",
	}
	for _, marker := range markers {
		if strings.Contains(t, marker) {
			return true
		}
	}
	return false
}

// shouldRetryVisionAnswer covers both explicit refusals and the other failure
// shape seen from the upstream model: returning only the hidden answer marker.
// An empty answer is only treated as a vision failure when the latest user
// turn really contains an attachment, so ordinary empty tool/final-summary
// recovery keeps its existing behavior.
func shouldRetryVisionAnswer(messages []officialtypes.APIMessage, text string) bool {
	return latestUserHasAttachment(messages) &&
		(strings.TrimSpace(text) == "" || looksLikeAttachmentAccessRefusal(text))
}

func incompleteAgentResponse(text string) bool {
	return strings.TrimSpace(text) == "" || looksLikeDeferredToolAction(text)
}

func lastUserIndex(messages []officialtypes.APIMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	return -1
}

func hasToolCallSinceLastUser(messages []officialtypes.APIMessage) bool {
	start := lastUserIndex(messages)
	for i := start + 1; i < len(messages); i++ {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
			return true
		}
	}
	return false
}

func conversationRequiresContentWork(messages []officialtypes.APIMessage) bool {
	i := lastUserIndex(messages)
	if i < 0 {
		return false
	}
	if textLooksLikeContentTask(normalizeIntentText(messages[i].Text())) {
		return true
	}
	// For an implicit follow-up, let the model classify intent semantically.
	// Once it actually emits a tool call, inherit the recent code/game/web
	// context and apply the stronger write+verify completion gate.
	return hasToolCallSinceLastUser(messages) && previousContentTask(messages, i)
}

func textLooksLikeContentTask(text string) bool {
	return contentTaskMatch(text) != ""
}

func contentTaskMatch(text string) string {
	markers := []string{"game", "app", "web", "website", "html", "css", "javascript", "typescript", "code", "project", "repo", "component", "script"}
	for _, marker := range markers {
		if containsIntentWord(text, marker) {
			return "word:" + marker
		}
	}
	for _, ext := range []string{".html", ".htm", ".css", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".go", ".py", ".rs", ".java", ".kt", ".c", ".cpp", ".cc", ".h", ".hpp", ".cs", ".php", ".rb", ".vue", ".svelte"} {
		if containsFileExtension(text, ext) {
			return "ext:" + ext
		}
	}
	return ""
}

func containsFileExtension(text, ext string) bool {
	for searchFrom := 0; searchFrom < len(text); {
		relative := strings.Index(text[searchFrom:], ext)
		if relative < 0 {
			return false
		}
		end := searchFrom + relative + len(ext)
		if end == len(text) {
			return true
		}
		var next rune
		for _, r := range text[end:] {
			next = r
			break
		}
		// A real extension ends at a separator, quote, line/column marker,
		// query delimiter, or another suffix dot. Letters/digits and filename
		// joiners mean this was only a prefix (for example .c in .clothes).
		if !unicode.IsLetter(next) && !unicode.IsDigit(next) && next != '_' && next != '-' {
			return true
		}
		searchFrom = end
	}
	return false
}

func previousContentTask(messages []officialtypes.APIMessage, before int) bool {
	seenUsers := 0
	for i := before - 1; i >= 0 && seenUsers < 4; i-- {
		if messages[i].Role != "user" {
			continue
		}
		seenUsers++
		if textLooksLikeContentTask(normalizeIntentText(messages[i].Text())) {
			return true
		}
	}
	return false
}

// deferredResponseRequiresTool decides whether a future-tense assistant reply
// should be retried as a mandatory tool turn. It intentionally looks at the
// conversation context rather than keyword-matching only the latest follow-up.
func deferredResponseRequiresTool(messages []officialtypes.APIMessage) bool {
	if conversationRequestsAction(messages) || conversationRequestsMutation(messages) {
		return true
	}
	lastUser := lastUserIndex(messages)
	return previousContentTask(messages, lastUser) || hasSuccessfulMutationBefore(messages, lastUser)
}

// hasSuccessfulMutationBefore preserves artifact context across terse
// follow-ups. OpenCode sends the prior assistant tool_call and matching tool
// result back to the provider, which is stronger evidence than relying on the
// user to repeat words such as "game", "code", or a file extension.
func hasSuccessfulMutationBefore(messages []officialtypes.APIMessage, before int) bool {
	if before < 0 || before > len(messages) {
		before = len(messages)
	}
	for i := 0; i < before; i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if toolCallMutatesWorkspace(call) && toolCallCompletedSuccessfully(messages, i, call.ID) {
				return true
			}
		}
	}
	return false
}

func containsIntentWord(text, word string) bool {
	padded := " " + strings.TrimSpace(text) + " "
	return strings.Contains(padded, " "+word+" ") ||
		strings.Contains(padded, " "+word+".") ||
		strings.Contains(padded, " "+word+",") ||
		strings.Contains(padded, " "+word+":") ||
		strings.Contains(padded, " "+word+"/") ||
		strings.Contains(padded, "/"+word+" ")
}

func toolResultLooksFailed(text string) bool {
	t := normalizeIntentText(text)
	for _, marker := range []string{"verification failed", "failed to", "file not found", "not found", "status error", "error:", "cannot ", "could not ", "exit code 1", "exit 1"} {
		if strings.Contains(t, marker) {
			return true
		}
	}
	return false
}

func toolCallCompletedSuccessfully(messages []officialtypes.APIMessage, assistantIndex int, callID string) bool {
	for i := assistantIndex + 1; i < len(messages); i++ {
		if messages[i].Role == "user" {
			break
		}
		if !messages[i].IsToolResult() {
			continue
		}
		if callID != "" && messages[i].ToolCallID != "" && messages[i].ToolCallID != callID {
			continue
		}
		return !toolResultLooksFailed(messages[i].Text())
	}
	return false
}

func latestToolResultFailed(messages []officialtypes.APIMessage) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].IsToolResult() {
			return toolResultLooksFailed(messages[i].Text())
		}
		if messages[i].Role == "user" {
			return false
		}
	}
	return false
}

func hasContentMutationToolCallSinceLastUser(messages []officialtypes.APIMessage) bool {
	start := lastUserIndex(messages)
	for i := start + 1; i < len(messages); i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if toolCallWritesContent(call) && toolCallCompletedSuccessfully(messages, i, call.ID) {
				return true
			}
		}
	}
	return false
}

func hasVerificationAfterContentMutation(messages []officialtypes.APIMessage) bool {
	start := lastUserIndex(messages)
	lastMutation := -1
	for i := start + 1; i < len(messages); i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if toolCallWritesContent(call) && toolCallCompletedSuccessfully(messages, i, call.ID) {
				lastMutation = i
			}
		}
	}
	if lastMutation < 0 {
		return false
	}
	for i := lastMutation + 1; i < len(messages); i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if toolCallVerifiesWork(call) && toolCallCompletedSuccessfully(messages, i, call.ID) {
				return true
			}
		}
	}
	return false
}

func toolCallWritesContent(call officialtypes.ToolCallRef) bool {
	name := normalizeIntentText(call.Function.Name)
	for _, marker := range []string{"write", "edit", "apply_patch", "patch", "write_file", "create_file", "str_replace", "replace"} {
		if name == marker || strings.Contains(name, marker) {
			return true
		}
	}
	if name != "bash" && name != "shell" && name != "terminal" && name != "exec" && name != "run_command" {
		return false
	}
	args := strings.ToLower(call.Function.Arguments)
	markers := []string{"set-content", "add-content", "out-file", "writealltext", "writeallbytes", "cat >", "cat >>", "tee ", " > ", ">>"}
	for _, marker := range markers {
		if strings.Contains(args, marker) {
			return true
		}
	}
	return false
}

func toolCallVerifiesWork(call officialtypes.ToolCallRef) bool {
	name := normalizeIntentText(call.Function.Name)
	for _, marker := range []string{"read", "view", "open", "test", "check", "lint", "diagnostic"} {
		if name == marker || strings.Contains(name, marker) {
			return true
		}
	}
	if name != "bash" && name != "shell" && name != "terminal" && name != "exec" && name != "run_command" {
		return false
	}
	args := strings.ToLower(call.Function.Arguments)
	markers := []string{"test-path", "get-content", "get-item", "node --check", "npm test", "npm run", "pnpm test", "yarn test", "pytest", "go test", "cargo test", "dotnet test", "curl ", "invoke-webrequest", "start-process", "python ", "node "}
	for _, marker := range markers {
		if strings.Contains(args, marker) {
			return true
		}
	}
	return false
}

func hasMutationToolCallSinceLastUser(messages []officialtypes.APIMessage) bool {
	start := lastUserIndex(messages)
	for i := start + 1; i < len(messages); i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if toolCallMutatesWorkspace(call) && toolCallCompletedSuccessfully(messages, i, call.ID) {
				return true
			}
		}
	}
	return false
}

func toolCallMutatesWorkspace(call officialtypes.ToolCallRef) bool {
	name := normalizeIntentText(call.Function.Name)
	for _, marker := range []string{"write", "edit", "apply_patch", "patch", "write_file", "create_file", "str_replace", "replace"} {
		if name == marker || strings.Contains(name, marker) {
			return true
		}
	}
	if name != "bash" && name != "shell" && name != "terminal" && name != "exec" && name != "run_command" {
		return false
	}
	args := strings.ToLower(call.Function.Arguments)
	markers := []string{"set-content", "add-content", "out-file", "new-item", "copy-item", "move-item", "remove-item", "rename-item", "mkdir ", "touch ", "tee ", "cat >", "cat >>", " > ", ">>", "writealltext", "writeallbytes", "cp ", "mv ", "rm ", "del ", "npm install", "pnpm add", "yarn add", "pip install"}
	for _, marker := range markers {
		if strings.Contains(args, marker) {
			return true
		}
	}
	return false
}
func conversationRequestsMutation(messages []officialtypes.APIMessage) bool {
	i := lastUserIndex(messages)
	if i < 0 {
		return false
	}
	text := stripNegatedMutationPhrases(normalizeIntentText(messages[i].Text()))
	markers := []string{"hay tao ", "tao di", "tao game", "tao file", "tao app", "tao web", "tao project", "tao thu muc", "lam di", "lam luon", "tien hanh", "lam game", "lam app", "lam web", "viet ", "sua ", "them ", "xoa ", "cap nhat code", "fix it", "fix this", "implement ", "create ", "make ", "write ", "edit ", "modify ", "change ", "add ", "remove ", "delete ", "refactor", "rename ", "move file"}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	confirmations := map[string]bool{"ok": true, "okay": true, "ok lam di": true, "lam di": true, "duoc": true, "tiep di": true, "continue": true, "go ahead": true}
	if confirmations[text] && i > 0 {
		for j := i - 1; j >= 0; j-- {
			if messages[j].Role != "assistant" {
				continue
			}
			if assistantPromisesMutation(messages[j].Text()) {
				return true
			}
			break
		}
	}
	return false
}

// stripNegatedMutationPhrases prevents safety constraints such as "do not
// modify any files" from being mistaken for a request to modify the workspace.
// Only the negated verb is removed so a positive instruction elsewhere in the
// same message (for example "fix the code, but do not edit tests") still wins.
func stripNegatedMutationPhrases(text string) string {
	for _, phrase := range []string{
		"do not modify", "don't modify", "dont modify", "without modifying",
		"do not change", "don't change", "dont change", "without changing",
		"do not edit", "don't edit", "dont edit", "without editing",
		"do not write", "don't write", "dont write", "without writing",
		"do not create", "don't create", "dont create", "without creating",
		"do not remove", "don't remove", "dont remove",
		"do not delete", "don't delete", "dont delete",
		"khong sua", "dung sua", "khong thay doi", "dung thay doi",
		"khong viet", "dung viet", "khong tao", "dung tao",
		"khong xoa", "dung xoa",
	} {
		text = strings.ReplaceAll(text, phrase, "")
	}
	return strings.Join(strings.Fields(text), " ")
}

func assistantPromisesMutation(text string) bool {
	t := normalizeIntentText(text)
	markers := []string{"i'll fix", "i will fix", "i'll implement", "i will implement", "i'll create", "i will create", "i'll write", "i will write", "tao se sua", "tao se lam", "tao se tao", "tao se viet", "minh se sua", "minh se lam", "minh se tao", "minh se viet"}
	for _, marker := range markers {
		if strings.Contains(t, marker) {
			return true
		}
	}
	return false
}
func userExplicitlyRequestsTool(messages []officialtypes.APIMessage) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		t := normalizeIntentText(messages[i].Text())
		for _, marker := range []string{"do not use a tool", "don't use a tool", "do not use tools", "khong dung tool", "dung dung tool"} {
			if strings.Contains(t, marker) {
				return false
			}
		}
		markers := []string{"must use the shell", "must use shell", "must use the bash tool", "must use bash", "must use a tool", "use the shell tool", "use the bash tool", "use shell", "use bash", "use a tool", "call the tool", "run a command", "execute a command", "phai dung shell", "phai dung bash", "phai dung tool", "dung shell", "dung bash", "goi tool", "chay lenh"}
		for _, marker := range markers {
			if strings.Contains(t, marker) {
				return true
			}
		}
		return false
	}
	return false
}

func conversationRequestsAction(messages []officialtypes.APIMessage) bool {
	i := lastUserIndex(messages)
	if i < 0 {
		return false
	}
	text := normalizeIntentText(messages[i].Text())
	markers := []string{"lam di", "lam luon", "tien hanh", "sua di", "sua luon", "them vao", "them vo", "xoa di", "tao di", "chay di", "test di", "build di", "deploy di", "commit di", "push di", "kiem tra di", "check di", "doc file", "mo file", "xem repo", "kiem tra repo", "xem cau truc", "sua code", "them code", "update code", "cap nhat code", "fix it", "fix this", "do it", "go ahead", "implement it", "implement this", "add it", "remove it", "delete it", "create it", "run it", "test it", "build it", "deploy it", "commit it", "push it", "check the repo", "inspect the repo", "read the file", "open the file", "modify the code", "change the code", "update the code", "refactor", "rename", "move the file"}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	confirmations := map[string]bool{"ok": true, "okay": true, "ok lam di": true, "lam di": true, "u": true, "uh": true, "duoc": true, "duoc lam di": true, "tiep di": true, "continue": true, "go ahead": true}
	if confirmations[text] && i > 0 {
		for j := i - 1; j >= 0; j-- {
			if messages[j].Role != "assistant" {
				continue
			}
			if assistantPromisesAction(messages[j].Text()) {
				return true
			}
			break
		}
	}
	return false
}

func assistantPromisesAction(text string) bool {
	t := normalizeIntentText(text)
	markers := []string{"i'll fix", "i will fix", "i'll implement", "i will implement", "i'll add", "i will add", "i'll remove", "i will remove", "i'll create", "i will create", "i'll run", "i will run", "i'll test", "i will test", "i'll build", "i will build", "i'll deploy", "i will deploy", "i'll commit", "i will commit", "i'll push", "i will push", "i'll inspect", "i will inspect", "tao se sua", "tao se lam", "tao se them", "tao se xoa", "tao se tao", "tao se chay", "tao se test", "tao se build", "tao se deploy", "tao se commit", "tao se push", "toi se sua", "toi se lam", "toi se them", "toi se tao", "toi se chay", "minh se sua", "minh se lam", "minh se them", "minh se tao", "minh se chay"}
	for _, marker := range markers {
		if strings.Contains(t, marker) {
			return true
		}
	}
	return false
}

func looksLikeDeferredToolAction(text string) bool {
	if text == "" {
		return false
	}
	t := " " + normalizeIntentText(text) + " "
	// This deliberately does not classify *what* the action is. It only catches
	// a response that postpones work into the future instead of either executing
	// a host tool now or answering the user now.
	for _, marker := range []string{" i will ", " i'll ", " i am going to ", " i'm going to ", " let me ", " minh se ", " toi se ", " tao se ", " de minh ", " de toi ", " de tao "} {
		if strings.Contains(t, marker) {
			return true
		}
	}
	return false
}

// appendToolDebugLog writes each tool-parse attempt and parsed calls to the configured debug log.
func appendToolDebugLog(path string, attempt int, text string, calls []officialtypes.ToolCall) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	callsJSON, _ := json.Marshal(calls)
	fmt.Fprintf(f, "\n=== attempt %d ===\ntext: %s\ncalls: %s\n", attempt, text, string(callsJSON))
}

// ── Responses 流式事件构造器 ──

func responsesCreatedEvent(respID, model string) string {
	evt := map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id": respID, "object": "response", "created_at": time.Now().Unix(),
			"model": model, "status": "in_progress",
		},
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesOutputItemAddedEvent(outputIndex int, itemID, itemType string) string {
	evt := map[string]interface{}{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item": map[string]interface{}{
			"id": itemID, "type": itemType, "status": "in_progress",
		},
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesOutputItemDoneEvent(outputIndex int, itemID, itemType, text string) string {
	item := map[string]interface{}{
		"id": itemID, "type": itemType, "status": "completed",
	}
	if itemType == "message" {
		item["role"] = "assistant"
		item["content"] = []map[string]interface{}{
			{"type": "output_text", "text": text},
		}
	} else if itemType == "reasoning" {
		item["content"] = []map[string]interface{}{
			{"type": "reasoning_text", "text": text},
		}
	}
	evt := map[string]interface{}{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
		"item":         item,
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesFailedEvent(msg string) string {
	evt := map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"error": map[string]interface{}{
				"message": msg, "type": "server_error",
			},
		},
	}
	b, _ := json.Marshal(evt)
	return string(b)
}

func responsesCompletedEvent(resp officialtypes.ResponsesResponse) string {
	evt := map[string]interface{}{
		"type":     "response.completed",
		"response": resp,
	}
	b, _ := json.Marshal(evt)
	return string(b)
}
