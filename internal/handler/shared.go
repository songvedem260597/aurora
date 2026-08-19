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
		acct, err := pool.Acquire(accounts.TypeFree)
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
	acct, err := pool.Acquire(accounts.TypeFree)
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
		if request.ToolChoice.Type == "any" || request.ToolChoice.ForcedFunctionName() != "" {
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
	return looksLikeSandboxRefusal(text) || looksLikeDeferredToolAction(text)
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
	text := normalizeIntentText(messages[i].Text())
	markers := []string{"game", "app", "web", "website", "html", "css", "javascript", "typescript", "code", "project", "repo", "component", "script"}
	for _, marker := range markers {
		if containsIntentWord(text, marker) {
			return true
		}
	}
	for _, ext := range []string{".html", ".htm", ".css", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".go", ".py", ".rs", ".java", ".kt", ".c", ".cpp", ".cc", ".h", ".hpp", ".cs", ".php", ".rb", ".vue", ".svelte"} {
		if strings.Contains(text, ext) {
			return true
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

func hasContentMutationToolCallSinceLastUser(messages []officialtypes.APIMessage) bool {
	start := lastUserIndex(messages)
	for i := start + 1; i < len(messages); i++ {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if toolCallWritesContent(call) {
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
			if toolCallWritesContent(call) {
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
			if toolCallVerifiesWork(call) {
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
			if toolCallMutatesWorkspace(call) {
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
	text := normalizeIntentText(messages[i].Text())
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
		markers := []string{"must use the shell", "must use shell", "must use bash", "must use a tool", "use the shell tool", "use shell", "use bash", "use a tool", "call the tool", "run a command", "execute a command", "phai dung shell", "phai dung bash", "phai dung tool", "dung shell", "dung bash", "goi tool", "chay lenh"}
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
			if looksLikeDeferredToolAction(messages[j].Text()) || assistantPromisesAction(messages[j].Text()) {
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
	t := normalizeIntentText(text)
	markers := []string{"i'll inspect", "i will inspect", "let me inspect", "i'm going to inspect", "i'll check", "i will check", "let me check", "i'm going to check", "i'll read", "i will read", "let me read", "i'll open", "i will open", "let me open", "i'll run", "i will run", "let me run", "i'll execute", "i will execute", "i'll create", "i will create", "i'll edit", "i will edit", "i'll modify", "i will modify", "i'll test", "i will test", "i'll build", "i will build", "i'll start by", "i will start by", "tao se xem", "tao se kiem tra", "tao se doc", "tao se mo", "tao se chay", "tao se tao", "tao se sua", "tao se test", "tao se build", "tao bat dau bang", "toi se xem", "toi se kiem tra", "toi se doc", "toi se chay", "toi se tao", "toi se sua", "minh se xem", "minh se kiem tra", "minh se doc", "minh se chay", "minh se tao", "minh se sua", "de tao xem", "de tao kiem tra", "de toi xem", "de toi kiem tra", "de minh xem", "de minh kiem tra", "se xem cau truc", "se kiem tra repo", "se doc file", "se chay lenh", "dang tao", "dang sua", "dang lam"}
	for _, marker := range markers {
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
