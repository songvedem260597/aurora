package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	chatgptrequestconverter "aurora/conversion/requests/chatgpt"
	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	"aurora/internal/httpstream"
	"aurora/internal/toolcall"
	chatgpt_types "aurora/typings/chatgpt"
	officialtypes "aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ChatHandler struct {
	accountPool *accounts.Pool
	sessions    *SessionManager
	cfg         *config.Config
}

func NewChatHandler(pool *accounts.Pool, cfg *config.Config) *ChatHandler {
	return &ChatHandler{
		accountPool: pool,
		sessions:    NewSessionManager(),
		cfg:         cfg,
	}
}

func (h *ChatHandler) Nightmare(c *gin.Context) {
	var original_request officialtypes.APIRequest
	err := c.BindJSON(&original_request)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Request must be proper JSON",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    err.Error(),
		}})
		return
	}
	if len(original_request.Messages) == 0 {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Missing required parameter: messages",
			"type":    "invalid_request_error",
			"param":   "messages",
			"code":    "missing_required_parameter",
		}})
		return
	}

	account, accountStatus, err := resolveAccount(c, h.accountPool, h.cfg, original_requestHasFiles(original_request))
	if err != nil {
		if errors.Is(err, accounts.ErrAttachmentLimited) {
			respondAttachmentQuotaError(c)
			return
		}
		c.JSON(accountStatus, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "authorization_error",
			"param":   "Authorization",
			"code":    400,
		}})
		return
	}
	if account == nil {
		c.JSON(400, gin.H{"error": "Not Account Found."})
		c.Abort()
		return
	}

	proxyUrl := account.Proxy
	input_tokens := countMessagesTokens(original_request.Messages)

	uid := uuid.NewString()
	// 优先用 account.Client（bootstrap.InitClient 时已绑 fingerprint + proxy）
	// 只有在 account.Client 为 nil（理论上不应发生）才 fallback 到 setupClientWithProxy
	var client *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		client = c
	} else {
		client = setupClientWithProxy(proxyUrl)
	}

	// 工具调用模式判定
	toolsEnabled := requestToolCallingEnabled(&original_request, h.cfg)
	// OpenCode includes its host tools on every turn, including a plain request
	// to describe an image that is already attached. That turn does not need the
	// buffered tool-protocol classifier: route it through the normal streaming
	// path so the first visible text reaches OpenCode as soon as upstream emits
	// it. Coding/edit requests that happen to include an image keep tool mode.
	if toolsEnabled && prepareDirectInformationalAttachment(&original_request) {
		toolsEnabled = false
	}
	toolStreamRequested := original_request.Stream
	if toolsEnabled && h.cfg.StreamMode {
		original_request.Stream = false
	}
	reqModel := original_request.Model
	if reqModel == "" {
		reqModel = "auto"
	}

	// The tool handler performs its own conversion because it may strip the
	// host-tool protocol for informational image turns. Branch before the
	// generic conversion so an inline image is not uploaded twice.
	var clientState *chatgpt.ChatClientState
	if toolsEnabled {
		h.handleToolCalling(c, &original_request, &client, account, &clientState, &reqModel, &uid, &proxyUrl, &input_tokens, toolStreamRequested)
		return
	}

	// Convert the chat request to a ChatGPT request
	translated_request, err := chatgptrequestconverter.ConvertAPIRequest(original_request, account, proxyUrl, client)
	if err != nil {
		respondRequestConversionError(c, err)
		return
	}

	// 按 conversationID 复用 ChatClientState
	if translated_request.ConversationID != "" {
		clientState = h.sessions.Get(translated_request.ConversationID)
	}
	if clientState == nil {
		clientState = chatgpt.NewChatClientState()
	}
	clientState.ConversationID = translated_request.ConversationID
	clientState.ParentMessageID = translated_request.ParentMessageID

	response, wsConn, turnStile, status, err := conversationClientOrder(&client, account, translated_request, proxyUrl, original_request.Stream, clientState, h.accountPool)
	if err != nil {
		c.JSON(status, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "request_conversion_error",
			"param":   "model",
			"code":    "request_conversion_error",
		}})
		return
	}
	defer response.Body.Close()
	if chatgpt.Handle_request_error(c, response) {
		if wsConn != nil {
			wsConn.Close()
			wsConn = nil
		}
		return
	}
	var full_response string
	var full_thinking string
	var conversationID string
	var sentinel []map[string]interface{}
	var stopSent bool
	pingSent := false

	// 记录请求开始时间，用于 TTFT / total-time 计时
	startTime := time.Now()
	ttftSet := false
	var ttftMs int64

	// 提取 instructions / input 用于缓存模拟（与 Responses 路径一致）
	var instructions string
	var inputTextParts []string
	for _, msg := range original_request.Messages {
		if msg.Role == "system" {
			instructions += msg.Text()
		} else {
			inputTextParts = append(inputTextParts, msg.Text())
		}
	}
	inputText := strings.Join(inputTextParts, "\n")
	cacheWriteTokens, cachedTokens := RecordCache(translated_request.ConversationID, instructions, inputText)

	if !h.cfg.StreamMode {
		original_request.Stream = false
	}
	if original_request.Stream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
	}
	for i := h.cfg.MaxContinueCount; i > 0; i-- {
		var continue_info *chatgpt.ContinueInfo
		result := chatgpt.HandlerDetailedWithOptions(c, response, client, account, uid, translated_request, original_request.Stream, reqModel, chatgpt.HandlerDetailedOptions{
			Websocket:        wsConn,
			ClientState:      clientState,
			ArtifactDelivery: original_request.ArtifactDelivery,
			ProxyURL:         proxyUrl,
		})
		wsConn = nil
		continue_info = result.Continue
		full_response += result.Text
		full_thinking += result.ThinkingText
		// 首个输出 token 到达时记录 TTFT（text chunk 已在 HandlerDetailedWithOptions 内写出）
		if result.Text != "" && !ttftSet {
			ttftSet = true
			ttftMs = time.Since(startTime).Milliseconds()
		}
		if result.ConversationID != "" {
			conversationID = result.ConversationID
			h.sessions.Register(conversationID, clientState)
			if !pingSent && turnStile != nil {
				pingSent = true
				lastMsgID := result.ParentMessageID
				pingClient := client
				pingAccount := account
				pingTurnStile := turnStile
				go func() {
					perr := chatgpt.POSTSentinelPing(pingClient, pingAccount, pingTurnStile, conversationID, lastMsgID, clientState)
					if h.cfg.DebugSentinel {
						fmt.Printf("[sentinel-ping] conv=%s lastMsg=%s err=%v\n", conversationID, lastMsgID, perr)
					}
				}()
			}
		}
		sentinel = append(sentinel, result.Sentinel...)
		if result.StopSent {
			stopSent = true
		}
		parentMessageID := result.ParentMessageID
		if continue_info != nil {
			parentMessageID = continue_info.ParentID
		}
		clientState.NoteTurnResult(result.ConversationID, parentMessageID)
		if continue_info == nil {
			break
		}
		translated_request.Messages = nil
		translated_request.Action = "continue"
		translated_request.ConversationID = continue_info.ConversationID
		translated_request.ParentMessageID = continue_info.ParentID

		response, wsConn, _, status, err = conversationClientOrder(&client, account, translated_request, proxyUrl, original_request.Stream, clientState, h.accountPool)
		if err != nil {
			c.JSON(status, gin.H{"error": gin.H{
				"message": err.Error(),
				"type":    "request_conversion_error",
				"param":   "model",
				"code":    "request_conversion_error",
			}})
			return
		}
		defer response.Body.Close()
		if chatgpt.Handle_request_error(c, response) {
			if wsConn != nil {
				wsConn.Close()
				wsConn = nil
			}
			return
		}
	}
	if c.Writer.Status() != 200 {
		return
	}
	if !original_request.Stream {
		output_tokens := util.CountToken(full_response)
		c.JSON(200, officialtypes.NewChatCompletionWithMetadataAndReasoning(full_response, full_thinking, input_tokens, output_tokens, reqModel, conversationID, sentinel))
	} else {
		if original_request.StreamOptions != nil && original_request.StreamOptions.IncludeUsage {
			output_tokens := util.CountToken(full_response)
			msSinceStart := time.Since(startTime).Milliseconds()
			httpstream.WriteUsageChunk(c, reqModel, input_tokens, output_tokens, cachedTokens, cacheWriteTokens, msSinceStart, ttftMs, ttftSet)
		}
		writeChatCompletionStreamDone(c, stopSent, reqModel, conversationID)
	}
}

func (h *ChatHandler) Responses(c *gin.Context) {
	var responsesRequest officialtypes.ResponsesAPIRequest
	err := c.BindJSON(&responsesRequest)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Request must be proper JSON",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    err.Error(),
		}})
		return
	}

	original_request, err := responsesRequest.ToAPIRequest()
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
			"param":   "input",
			"code":    "invalid_request_error",
		}})
		return
	}

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, original_requestHasFiles(original_request))
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "authorization_error",
			"param":   "Authorization",
			"code":    400,
		}})
		return
	}
	if account == nil {
		c.JSON(400, gin.H{"error": "Not Account Found."})
		c.Abort()
		return
	}
	if !account.Type.Satisfies(accounts.CapResponses) {
		c.JSON(403, gin.H{"error": "Responses API requires a logged-in ChatGPT account."})
		return
	}

	proxyUrl := account.Proxy
	input_tokens := 0
	for _, message := range original_request.Messages {
		input_tokens += util.CountToken(message.Text())
	}

	uid := uuid.NewString()
	// 优先用 account.Client（bootstrap.InitClient 时已绑 fingerprint + proxy）
	var client *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		client = c
	} else {
		client = setupClientWithProxy(proxyUrl)
	}

	translated_request, err := chatgptrequestconverter.ConvertAPIRequest(original_request, account, proxyUrl, client)
	if err != nil {
		respondRequestConversionError(c, err)
		return
	}

	// 按 conversationID 复用 ChatClientState，保持 DeviceID/SessionID 一致
	var clientState *chatgpt.ChatClientState
	if translated_request.ConversationID != "" {
		clientState = h.sessions.Get(translated_request.ConversationID)
	}
	if clientState == nil {
		clientState = chatgpt.NewChatClientState()
	}
	clientState.ConversationID = translated_request.ConversationID
	clientState.ParentMessageID = translated_request.ParentMessageID
	reqModel := original_request.Model
	if reqModel == "" {
		reqModel = "auto"
	}

	// 提取 instructions / input 用于缓存模拟
	var instructions string
	var inputTextParts []string
	for _, msg := range original_request.Messages {
		if msg.Role == "system" {
			instructions += msg.Text()
		} else {
			inputTextParts = append(inputTextParts, msg.Text())
		}
	}
	inputText := strings.Join(inputTextParts, "\n")
	cacheWriteTokens, cachedTokens := RecordCache(translated_request.ConversationID, instructions, inputText)

	streamRequested := responsesRequest.Stream && h.cfg.StreamMode

	// 非流式路径：保持原有行为，使用新的 NewResponsesResponse 签名（含 reasoning + cache）
	if !streamRequested {
		response, wsConn, _, status, err := conversationClientOrder(&client, account, translated_request, proxyUrl, false, clientState, h.accountPool)
		if err != nil {
			c.JSON(status, gin.H{"error": gin.H{
				"message": err.Error(),
				"type":    "request_conversion_error",
				"param":   "model",
				"code":    "request_conversion_error",
			}})
			return
		}
		defer response.Body.Close()
		if chatgpt.Handle_request_error(c, response) {
			if wsConn != nil {
				wsConn.Close()
				wsConn = nil
			}
			return
		}

		var full_response string
		var full_thinking string
		var conversationID string
		for i := h.cfg.MaxContinueCount; i > 0; i-- {
			var continue_info *chatgpt.ContinueInfo
			result := chatgpt.HandlerDetailedWithOptions(c, response, client, account, uid, translated_request, false, reqModel, chatgpt.HandlerDetailedOptions{
				Websocket:   wsConn,
				ClientState: clientState,
			})
			wsConn = nil
			full_response += result.Text
			full_thinking += result.ThinkingText
			parentMessageID := result.ParentMessageID
			continue_info = result.Continue
			if continue_info != nil {
				parentMessageID = continue_info.ParentID
			}
			clientState.NoteTurnResult(result.ConversationID, parentMessageID)
			if result.ConversationID != "" {
				conversationID = result.ConversationID
				h.sessions.Register(conversationID, clientState)
			}
			if continue_info == nil {
				break
			}
			translated_request.Messages = nil
			translated_request.Action = "continue"
			translated_request.ConversationID = continue_info.ConversationID
			translated_request.ParentMessageID = continue_info.ParentID

			response, wsConn, _, status, err = conversationClientOrder(&client, account, translated_request, proxyUrl, false, clientState, h.accountPool)
			if err != nil {
				c.JSON(status, gin.H{"error": gin.H{
					"message": err.Error(),
					"type":    "request_conversion_error",
					"param":   "model",
					"code":    "request_conversion_error",
				}})
				return
			}
			defer response.Body.Close()
			if chatgpt.Handle_request_error(c, response) {
				if wsConn != nil {
					wsConn.Close()
					wsConn = nil
				}
				return
			}
		}
		if c.Writer.Status() != 200 {
			return
		}

		output_tokens := util.CountToken(full_response)
		reasoning_tokens := util.CountToken(full_thinking)
		responsesResponse := officialtypes.NewResponsesResponse(full_response, full_thinking, input_tokens, output_tokens, reasoning_tokens, cachedTokens, cacheWriteTokens, reqModel)
		c.JSON(200, responsesResponse)
		return
	}

	// ── 流式路径 ──
	startTime := time.Now()
	respID := "resp_" + uuid.NewString()
	reasoningItemID := "rs_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := c.Writer.(http.Flusher)

	// response.created
	c.Writer.WriteString("event: response.created\ndata: " + responsesCreatedEvent(respID, reqModel) + "\n\n")
	// output_item.added (reasoning, output_index 0)
	c.Writer.WriteString("event: response.output_item.added\ndata: " + responsesOutputItemAddedEvent(0, reasoningItemID, "reasoning") + "\n\n")
	// output_item.added (message, output_index 1)
	c.Writer.WriteString("event: response.output_item.added\ndata: " + responsesOutputItemAddedEvent(1, messageItemID, "message") + "\n\n")
	if flusher != nil {
		c.Writer.WriteHeader(200)
		flusher.Flush()
	}

	response, wsConn, _, _, err := conversationClientOrder(&client, account, translated_request, proxyUrl, true, clientState, h.accountPool)
	if err != nil {
		c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent(err.Error()) + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	defer response.Body.Close()
	if chatgpt.Handle_request_error(c, response) {
		if wsConn != nil {
			wsConn.Close()
			wsConn = nil
		}
		c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent("upstream error") + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	var full_response string
	var full_thinking string
	var conversationID string
	ttftSet := false
	var ttftMs int64

	for i := h.cfg.MaxContinueCount; i > 0; i-- {
		var continue_info *chatgpt.ContinueInfo
		result := chatgpt.HandlerDetailedWithOptions(c, response, client, account, uid, translated_request, true, reqModel, chatgpt.HandlerDetailedOptions{
			Websocket:   wsConn,
			ClientState: clientState,
		})
		wsConn = nil
		full_response += result.Text
		full_thinking += result.ThinkingText

		// 思维链增量
		if result.ThinkingText != "" {
			reasoningEvt := officialtypes.ResponsesReasoningDeltaEvent{
				Type:         "response.reasoning_text.delta",
				ItemID:       reasoningItemID,
				OutputIndex:  0,
				ContentIndex: 0,
				Delta:        result.ThinkingText,
			}
			c.Writer.WriteString("event: response.reasoning_text.delta\ndata: " + reasoningEvt.String() + "\n\n")
		}

		// 正文增量
		if result.Text != "" {
			if !ttftSet {
				ttftSet = true
				ttftMs = time.Since(startTime).Milliseconds()
			}
			textEvt := officialtypes.ResponsesTextDeltaEvent{
				Type:         "response.output_text.delta",
				ItemID:       messageItemID,
				OutputIndex:  1,
				ContentIndex: 0,
				Delta:        result.Text,
			}
			c.Writer.WriteString("event: response.output_text.delta\ndata: " + textEvt.String() + "\n\n")
		}

		if flusher != nil {
			flusher.Flush()
		}

		parentMessageID := result.ParentMessageID
		continue_info = result.Continue
		if continue_info != nil {
			parentMessageID = continue_info.ParentID
		}
		clientState.NoteTurnResult(result.ConversationID, parentMessageID)
		if result.ConversationID != "" {
			conversationID = result.ConversationID
			h.sessions.Register(conversationID, clientState)
		}
		if continue_info == nil {
			break
		}
		translated_request.Messages = nil
		translated_request.Action = "continue"
		translated_request.ConversationID = continue_info.ConversationID
		translated_request.ParentMessageID = continue_info.ParentID

		response, wsConn, _, _, err = conversationClientOrder(&client, account, translated_request, proxyUrl, true, clientState, h.accountPool)
		if err != nil {
			c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent(err.Error()) + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		defer response.Body.Close()
		if chatgpt.Handle_request_error(c, response) {
			if wsConn != nil {
				wsConn.Close()
				wsConn = nil
			}
			c.Writer.WriteString("event: response.failed\ndata: " + responsesFailedEvent("upstream error") + "\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
	}

	// output_item.done (reasoning)
	c.Writer.WriteString("event: response.output_item.done\ndata: " + responsesOutputItemDoneEvent(0, reasoningItemID, "reasoning", full_thinking) + "\n\n")
	// output_item.done (message)
	c.Writer.WriteString("event: response.output_item.done\ndata: " + responsesOutputItemDoneEvent(1, messageItemID, "message", full_response) + "\n\n")

	output_tokens := util.CountToken(full_response)
	reasoning_tokens := util.CountToken(full_thinking)
	responsesResponse := officialtypes.NewResponsesResponse(full_response, full_thinking, input_tokens, output_tokens, reasoning_tokens, cachedTokens, cacheWriteTokens, reqModel)
	// 在 response.completed 事件里附带 timing（HTTP headers 在首次 Flush 后不可写）
	responsesResponse.MsSinceStart = time.Since(startTime).Milliseconds()
	if ttftSet {
		responsesResponse.MsTTFT = ttftMs
	}
	// response.completed
	c.Writer.WriteString("event: response.completed\ndata: " + responsesCompletedEvent(responsesResponse) + "\n\n")
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeTimingHeader 在非流式响应中设置 timing 头部（仅非流式路径使用）。

func (h *ChatHandler) Files(c *gin.Context) {
	account, _, err := resolveAccount(c, h.accountPool, h.cfg, true)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Files API requires a logged-in ChatGPT access token.",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    "missing_access_token",
		}})
		return
	}
	if account == nil || account.Token == "" || !account.Type.Satisfies(accounts.CapFileUpload) {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Files API requires a logged-in ChatGPT access token.",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    "missing_access_token",
		}})
		return
	}

	formFile, err := c.FormFile("file")
	if err != nil {
		respondError(c, 400, err)
		return
	}
	file, err := formFile.Open()
	if err != nil {
		respondError(c, 400, err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		respondError(c, 400, err)
		return
	}
	if len(data) == 0 {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Uploaded file is empty",
			"type":    "invalid_request_error",
			"param":   "file",
			"code":    "empty_file",
		}})
		return
	}

	contentType := formFile.Header.Get("Content-Type")

	// 使用 account 绑定的 Client（有指纹 + 代理）；不存在则新建
	var fileClient *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		fileClient = c
	} else {
		fileClient = bogdanfinn.NewStdClient()
		fileClient.SetCookies("https://chatgpt.com", chatgpt.BasicCookies)
	}

	uploaded, status, err := chatgpt.UploadFile(fileClient, account, account.Proxy, formFile.Filename, contentType, data)
	if err != nil {
		c.JSON(status, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "file_upload_error",
			"param":   "file",
			"code":    "file_upload_error",
		}})
		return
	}
	uploaded.CreatedAt = time.Now().Unix()
	chatgpt.RegisterUploadedFile(uploaded)
	c.JSON(200, uploaded)
}

// handleToolCalling 工具调用模式的主流程（对齐 initialize/handlers.go:handleToolCalling）
func (h *ChatHandler) handleToolCalling(c *gin.Context, originalRequest *officialtypes.APIRequest, client **bogdanfinn.TlsClient, account *accounts.Account, clientState **chatgpt.ChatClientState, reqModel *string, uid *string, proxyUrl *string, inputTokens *int, streamRequested bool) {
	if account == nil || !account.Type.Satisfies(accounts.CapToolCalling) {
		c.JSON(403, gin.H{"error": "Tool calling requires a logged-in ChatGPT account."})
		return
	}
	tools := originalRequest.Tools
	maxRefusalRetries := h.cfg.RefusalRetries
	if maxRefusalRetries <= 0 {
		maxRefusalRetries = 3
	}

	upstreamRequest, informationalAttachment := toolUpstreamRequest(originalRequest)
	var baseTranslated chatgpt_types.ChatGPTRequest
	activateNextAttachmentAccount := func() bool {
		if h.accountPool == nil || !h.accountPool.ReportAttachmentLimited(account, time.Hour) {
			return false
		}
		next, acquireErr := h.accountPool.AcquireForAttachments(account.Type)
		if acquireErr != nil || next == nil {
			return false
		}
		account = next
		*proxyUrl = next.Proxy
		if accountClient, ok := next.Client.(*bogdanfinn.TlsClient); ok && accountClient != nil {
			*client = accountClient
		} else {
			*client = setupClientWithProxy(next.Proxy)
		}
		*clientState = chatgpt.NewChatClientState()
		return true
	}
	convertForCurrentAccount := func() error {
		translated, convertErr := chatgptrequestconverter.ConvertAPIRequest(upstreamRequest, account, *proxyUrl, *client)
		if convertErr != nil {
			return convertErr
		}
		baseTranslated = translated
		if baseTranslated.ConversationID != "" {
			*clientState = h.sessions.Get(baseTranslated.ConversationID)
		}
		if *clientState == nil {
			*clientState = chatgpt.NewChatClientState()
		}
		(*clientState).ConversationID = baseTranslated.ConversationID
		(*clientState).ParentMessageID = baseTranslated.ParentMessageID
		return nil
	}
	for {
		err := convertForCurrentAccount()
		if err == nil {
			break
		}
		if isAttachmentQuotaError(err.Error()) && activateNextAttachmentAccount() {
			continue
		}
		if isAttachmentQuotaError(err.Error()) {
			respondAttachmentQuotaError(c)
			return
		}
		respondRequestConversionError(c, err)
		return
	}

	var lastToolCalls []officialtypes.ToolCall
	var lastText string
	var lastConversationID string
	var lastSentinel []map[string]interface{}
	requireToolCall := shouldRequireToolCall(originalRequest, "")
	semanticRetry := false
	semanticFollowupContent := false
	completionRetry := false
	visionRetry := false
	if requireToolCall && maxRefusalRetries > 2 {
		maxRefusalRetries = 2
	}
	// Keep the streaming surface identical to a normal OpenAI-compatible
	// provider (such as 9Router): role/content/tool_calls/finish_reason only.
	// OpenCode renders Shell/Write/Edit rows from delta.tool_calls itself.
	progressStarted := false
	if logPath := h.cfg.DebugToolLog; logPath != "" {
		toolChoice := "<unset>"
		if originalRequest.ToolChoice != nil {
			toolChoice = originalRequest.ToolChoice.Type
			if forced := originalRequest.ToolChoice.ForcedFunctionName(); forced != "" {
				toolChoice += ":" + forced
			}
		}
		debugText := fmt.Sprintf(
			"require_tool_call=%v tool_choice=%s messages=%d prior_tool_call=%v content_task=%v mutation_task=%v action_task=%v",
			requireToolCall,
			toolChoice,
			len(originalRequest.Messages),
			hasToolCallSinceLastUser(originalRequest.Messages),
			conversationRequiresContentWork(originalRequest.Messages),
			conversationRequestsMutation(originalRequest.Messages),
			conversationRequestsAction(originalRequest.Messages) || userExplicitlyRequestsTool(originalRequest.Messages),
		)
		if len(originalRequest.Messages) > 0 {
			last := originalRequest.Messages[len(originalRequest.Messages)-1]
			debugText += fmt.Sprintf(" last_role=%s last_text=%q", last.Role, last.Text())
		}
		appendToolDebugLog(logPath, -1, debugText, nil)
	}

	for attempt := 0; attempt < maxRefusalRetries; attempt++ {
		if progressStarted && attempt > 0 {
			writeToolProgressSSE(c, *reqModel, "↻ Lượt trước chưa chạy tool thật — đang ép chọn lại tool/lệnh...")
		}
		translated := baseTranslated
		if len(tools) > 0 && !informationalAttachment {
			translated.AddMessage("user", "\n\n[HOST AGENT SEMANTIC CONTRACT: Infer the latest user's intent from the meaning of the FULL conversation, not from keywords. Start your response with EXACTLY ONE hidden intent marker. Use <agent_intent>action</agent_intent> when the user wants any real host/workspace action (including implicit follow-ups that refer to an artifact created earlier); immediately follow it with the appropriate <tool_call> block(s) and no planning prose. Use <agent_intent>answer</agent_intent> when the user only wants information, explanation, opinion, clarification, or analysis/description of content already attached in the latest turn; follow it with the complete answer in normal text and do not call tools. Client-generated helper text saying an attachment/file was read or loaded is preprocessing context, not a user request for host execution. Never reply with a future promise such as 'I will...' instead of acting or answering now.]")
		}
		if requireToolCall {
			retrySuffix := "\n\n[HOST TOOL PROTOCOL OVERRIDE: Do NOT look for a native ChatGPT bash/shell/file tool. The surrounding OpenCode host intercepts <tool_call> blocks from your TEXT response and executes them on the user's REAL machine. Your job is only to emit the protocol block; the host performs the command and sends the real result back on the next turn. Therefore never say the tool is unavailable, never guess command output or paths, and never describe what you plan to inspect. Respond with ONLY one or more <tool_call> blocks, starting immediately with '<tool_call>'.]"
			contentTask := conversationRequiresContentWork(originalRequest.Messages) || semanticFollowupContent
			contentMutationRequired := contentTask && !hasContentMutationToolCallSinceLastUser(originalRequest.Messages)
			verificationRequired := contentTask && !contentMutationRequired && !hasVerificationAfterContentMutation(originalRequest.Messages)
			mutationRequired := !contentTask && conversationRequestsMutation(originalRequest.Messages) && !hasMutationToolCallSinceLastUser(originalRequest.Messages)
			recoveryRequired := latestToolResultFailed(originalRequest.Messages)
			if !contentMutationRequired && !verificationRequired && !mutationRequired && !recoveryRequired {
				if forced := originalRequest.ToolChoice.ForcedFunctionName(); forced == "" {
					if example := toolcall.FirstToolCallExample(tools, toolcall.ExtractWorkingDir(originalRequest.Messages)); example != "" {
						retrySuffix += "\nThe host accepts this exact style; emit a call like this now:\n" + example
					}
				}
			}
			if recoveryRequired {
				retrySuffix += "\nThe previous host tool FAILED. Do not retry the same stale patch blindly and do not claim success. First emit a REAL read/inspect tool call for the affected target so the host returns the current contents/state. On the following turn, use that real result to retry the edit safely."
			} else if contentMutationRequired {
				retrySuffix += "\nThis is a coding/content task. Setup-only actions such as mkdir/New-Item Directory, pwd, ls, tree, Test-Path, skill/meta-guideline tools, or empty placeholder files DO NOT count as completing the task. Emit a REAL write/edit/apply_patch tool call (or a shell command that writes actual file contents) for the requested game/app/web/code NOW. Do not stop after setup or meta tools."
				if mutationPrompt := toolcall.MutationToolsPrompt(tools); mutationPrompt != "" {
					retrySuffix += "\nUse one of these ACTUAL mutation tools with its exact parameter schema; do not invent a tool name and do not answer in prose:\n" + mutationPrompt
				}
			} else if verificationRequired {
				retrySuffix += "\nThe requested code/content has been changed, but the task is NOT complete until it is verified. Run a REAL verification tool now (for example read/check/test/run/open the produced artifact or execute an appropriate test command). Do not claim completion before the host returns the verification result."
			} else if mutationRequired {
				retrySuffix += "\nThis is a create/modify task. Read-only inspection (pwd/ls/tree/Test-Path/read) does NOT count as doing the work. Emit a REAL write/edit/create tool call or a mutating shell command for the user's requested target NOW. Do not describe a plan, do not claim a file exists, and do not use a read-only tool as a substitute."
			}
			translated.AddMessage("user", retrySuffix)
		} else if visionRetry {
			translated.AddMessage("user", "\n\n[HOST VISION RETRY: The latest user turn already contains an image attachment that was successfully uploaded and included in this request. Inspect that attached image directly and answer the user's image question from the visual content. Do not claim that image input is unsupported, unavailable, unreadable, or missing unless the request itself returns a real attachment/file error. Answer normally in text and do not output an internal intent marker. Do not call a host tool just to inspect an attachment that is already present.]")
		} else if completionRetry {
			translated.AddMessage("user", "\n\n[HOST COMPLETION RETRY: The required host tool work has already completed and was verified. Your previous response contained only an internal intent marker. Start with <agent_intent>answer</agent_intent> and then give the user a concise final summary of the completed work. Do not call another tool and do not output an empty answer.]")
		} else if semanticRetry {
			translated.AddMessage("user", "\n\n[HOST INTENT RETRY: Your previous reply only deferred the next step instead of completing the turn. Re-evaluate the user's intent from the full conversation. If real host/workspace action is required, emit the correct <tool_call> immediately. If the user only wants information/explanation, answer it fully now in text. Do not reply with another promise about what you will do later.]")
		}

		response, wsConn, _, status, err := conversationClientOrder(client, account, translated, *proxyUrl, false, *clientState, h.accountPool)
		if err != nil {
			if progressStarted {
				writeToolErrorSSE(c, err.Error())
				return
			}
			c.JSON(status, gin.H{"error": gin.H{
				"message": err.Error(),
				"type":    "request_conversion_error",
				"param":   "model",
				"code":    "request_conversion_error",
			}})
			return
		}
		result := chatgpt.HandlerDetailedWithOptions(c, response, *client, account, *uid, translated, false, *reqModel, chatgpt.HandlerDetailedOptions{
			Websocket:            wsConn,
			ClientState:          *clientState,
			ArtifactDelivery:     originalRequest.ArtifactDelivery,
			ProxyURL:             *proxyUrl,
			ReturnUpstreamErrors: true,
		})
		response.Body.Close()
		if result.UpstreamError != "" {
			if isAttachmentQuotaError(result.UpstreamError) && activateNextAttachmentAccount() {
				for {
					convertErr := convertForCurrentAccount()
					if convertErr == nil {
						break
					}
					if isAttachmentQuotaError(convertErr.Error()) && activateNextAttachmentAccount() {
						continue
					}
					if isAttachmentQuotaError(convertErr.Error()) {
						respondAttachmentQuotaError(c)
						return
					}
					respondRequestConversionError(c, convertErr)
					return
				}
				// Account rotation is transport recovery, not a semantic retry.
				attempt--
				continue
			}
			if isAttachmentQuotaError(result.UpstreamError) {
				respondAttachmentQuotaError(c)
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
				"message": result.UpstreamError,
				"type":    "upstream_error",
				"code":    "upstream_error",
			}})
			return
		}

		agentIntent, cleanText := parseAgentIntent(result.Text)
		lastText = cleanText
		lastConversationID = result.ConversationID
		lastSentinel = result.Sentinel
		(*clientState).NoteTurnResult(result.ConversationID, result.ParentMessageID)
		if result.ConversationID != "" {
			h.sessions.Register(result.ConversationID, *clientState)
		}

		// 解析 <tool_call>{...}</tool_call>
		parser := toolcall.NewParser()
		_, calls := parser.Feed(cleanText)
		if len(calls) == 0 {
			_, extraCalls := parser.Flush()
			calls = append(calls, extraCalls...)
		}
		if len(calls) == 0 {
			calls = toolcall.RecoverFromText(cleanText, tools)
		}
		for i := range calls {
			calls[i].Index = i
		}
		if logPath := h.cfg.DebugToolLog; logPath != "" {
			appendToolDebugLog(logPath, attempt, result.Text, calls)
		}
		if len(calls) > 0 {
			if progressStarted {
				writeToolProgressSSE(c, *reqModel, toolProgressSummary(calls))
			}
			lastToolCalls = calls
			break
		}

		if !requireToolCall {
			requireToolCall = shouldRequireToolCall(originalRequest, cleanText)
		}
		// The semantic marker lets the upstream model classify terse follow-ups
		// without relying on keywords. Only escalate an action marker when no
		// tool has run for the latest user turn; completed write+verify turns are
		// allowed to proceed to their final summary.
		if !requireToolCall && agentIntentRequiresTool(agentIntent, originalRequest.Messages) {
			requireToolCall = true
			semanticFollowupContent = previousContentTask(originalRequest.Messages, lastUserIndex(originalRequest.Messages)) || hasSuccessfulMutationBefore(originalRequest.Messages, lastUserIndex(originalRequest.Messages))
		}
		if requireToolCall {
			if attempt < maxRefusalRetries-1 {
				fmt.Fprintf(os.Stderr, "[chatgpt] tool call required but none produced (attempt %d/%d), retrying\n", attempt+1, maxRefusalRetries)
			}
			continue
		}
		if shouldRetryVisionAnswer(originalRequest.Messages, cleanText) {
			visionRetry = true
			completionRetry = false
			if attempt < maxRefusalRetries-1 {
				fmt.Fprintf(
					os.Stderr,
					"[chatgpt] model failed to answer from an already-uploaded attachment (attempt %d/%d, empty=%t refusal=%t intent=%q raw_bytes=%d clean_bytes=%d), retrying vision answer\n",
					attempt+1,
					maxRefusalRetries,
					strings.TrimSpace(cleanText) == "",
					looksLikeAttachmentAccessRefusal(cleanText),
					agentIntent,
					len(result.Text),
					len(cleanText),
				)
				continue
			}
		}
		if looksLikeDeferredToolAction(cleanText) {
			semanticRetry = true
			userIndex := lastUserIndex(originalRequest.Messages)
			semanticFollowupContent = previousContentTask(originalRequest.Messages, userIndex)
			// A follow-up such as "make it vertical and add music" may not repeat
			// words like game/code/file. Once the model confirms the semantic
			// action by replying "I will ...", inherit the earlier content task and
			// escalate the retry to the mandatory host-tool protocol. Without this
			// transition the retry remains tool_choice=auto and can return a second
			// promise, which OpenCode renders as text instead of executing bash.
			if semanticFollowupContent || deferredResponseRequiresTool(originalRequest.Messages) {
				requireToolCall = true
			}
			if attempt < maxRefusalRetries-1 {
				fmt.Fprintf(os.Stderr, "[chatgpt] deferred response without tool or complete answer (attempt %d/%d), retrying intent classification\n", attempt+1, maxRefusalRetries)
				continue
			}
		}
		if strings.TrimSpace(cleanText) == "" && !visionRetry {
			completionRetry = true
			if attempt < maxRefusalRetries-1 {
				fmt.Fprintf(os.Stderr, "[chatgpt] empty response after removing internal intent marker (attempt %d/%d), retrying final summary\n", attempt+1, maxRefusalRetries)
				continue
			}
		}
		break
	}

	if len(lastToolCalls) > 0 {
		if streamRequested {
			writeToolCallingSSE(c, "", lastToolCalls, *reqModel, lastConversationID, progressStarted, toolStreamUsage(originalRequest, *inputTokens, lastText))
			return
		}
		c.JSON(200, officialtypes.NewChatCompletionWithToolCalls(
			lastText, "", lastToolCalls,
			*inputTokens, util.CountToken(lastText),
			*reqModel, lastConversationID, lastSentinel,
		))
		return
	}
	if requireToolCall {
		if progressStarted {
			writeToolErrorSSE(c, "action required a real tool call, but the model did not execute one")
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": "action required a real tool call, but the model did not execute one",
			"type":    "tool_call_error",
			"code":    "missing_tool_call",
		}})
		return
	}
	if visionRetry && shouldRetryVisionAnswer(originalRequest.Messages, lastText) {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": "model failed to answer from an attachment that was successfully uploaded",
			"type":    "vision_error",
			"code":    "attachment_access_refusal",
		}})
		return
	}
	if semanticRetry && looksLikeDeferredToolAction(lastText) {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": "model deferred the turn instead of either executing a host tool or answering the user",
			"type":    "agent_intent_error",
			"code":    "deferred_agent_response",
		}})
		return
	}
	if completionRetry && strings.TrimSpace(lastText) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": "model completed the tool turn but returned an empty final answer",
			"type":    "agent_intent_error",
			"code":    "empty_agent_response",
		}})
		return
	}
	outputTokens := util.CountToken(lastText)
	if streamRequested {
		if progressStarted {
			writeToolProgressSSE(c, *reqModel, "✅ Đã đủ bước thao tác/kiểm tra — đang trả kết quả cuối...")
		}
		writeToolCallingSSE(c, lastText, nil, *reqModel, lastConversationID, progressStarted, toolStreamUsage(originalRequest, *inputTokens, lastText))
		return
	}
	c.JSON(200, officialtypes.NewChatCompletionWithMetadata(lastText, *inputTokens, outputTokens, *reqModel, lastConversationID, lastSentinel))
}

func toolStreamUsage(request *officialtypes.APIRequest, inputTokens int, output string) *officialtypes.StreamUsage {
	if request == nil || request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
		return nil
	}
	outputTokens := util.CountToken(output)
	return &officialtypes.StreamUsage{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
	}
}

func writeToolCallingSSE(c *gin.Context, text string, calls []officialtypes.ToolCall, model, conversationID string, progressStarted bool, usage *officialtypes.StreamUsage) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	streamID := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()

	if !progressStarted {
		roleChunk := officialtypes.ChatCompletionChunk{
			ID:      streamID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []officialtypes.Choices{{
				Index: 0,
				Delta: officialtypes.Delta{Role: "assistant"},
			}},
		}
		c.Writer.WriteString("data: " + roleChunk.String() + "\n\n")
	}

	if len(calls) > 0 {
		// Match the raw 9Router OpenAI-compatible stream observed locally:
		// one delta.tool_calls chunk carries id/index/type/name/arguments,
		// followed by finish_reason=tool_calls.
		deltas := make([]officialtypes.ToolCallDelta, 0, len(calls))
		for _, call := range calls {
			deltas = append(deltas, officialtypes.ToolCallDelta{
				Index: call.Index,
				ID:    call.ID,
				Type:  call.Type,
				Function: officialtypes.ToolCallFuncDelta{
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				},
			})
		}
		chunk := officialtypes.NewToolCallChunk(model, deltas...)
		chunk.ID = streamID
		chunk.Created = created
		c.Writer.WriteString("data: " + chunk.String() + "\n\n")
		stop := officialtypes.NewToolCallStopChunk(model, conversationID)
		stop.ID = streamID
		stop.Created = created
		stop.Usage = usage
		c.Writer.WriteString("data: " + stop.String() + "\n\n")
	} else {
		if text != "" {
			chunk := officialtypes.NewChatCompletionChunk(text, model)
			chunk.ID = streamID
			chunk.Created = created
			c.Writer.WriteString("data: " + chunk.String() + "\n\n")
		}
		stop := officialtypes.StopChunkWithConversation("stop", model, conversationID)
		stop.ID = streamID
		stop.Created = created
		stop.Usage = usage
		c.Writer.WriteString("data: " + stop.String() + "\n\n")
	}

	c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}

func beginToolProgressSSE(c *gin.Context, model, status string) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	roleChunk := officialtypes.ChatCompletionChunk{
		ID: "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK", Object: "chat.completion.chunk", Created: 0, Model: model,
		Choices: []officialtypes.Choices{{Index: 0, Delta: officialtypes.Delta{Role: "assistant"}}},
	}
	c.Writer.WriteString("data: " + roleChunk.String() + "\n\n")
	chunk := officialtypes.NewChatCompletionChunk(status+"\n", model)
	c.Writer.WriteString("data: " + chunk.String() + "\n\n")
	c.Writer.Flush()
}

func writeToolProgressSSE(c *gin.Context, model, status string) {
	chunk := officialtypes.NewChatCompletionChunk(status+"\n", model)
	c.Writer.WriteString("data: " + chunk.String() + "\n\n")
	c.Writer.Flush()
}
func writeToolErrorSSE(c *gin.Context, message string) {
	payload, _ := json.Marshal(gin.H{"error": gin.H{"message": message, "type": "tool_call_error"}})
	c.Writer.WriteString("data: " + string(payload) + "\n\n")
	c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}
func toolProgressSummary(calls []officialtypes.ToolCall) string {
	if len(calls) == 0 {
		return "🔧 Tool execution requested"
	}
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, visibleToolProgress(call))
	}
	return strings.Join(parts, "\n")
}

func visibleToolProgress(call officialtypes.ToolCall) string {
	name := call.Function.Name
	args := map[string]interface{}{}
	_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
	for _, key := range []string{"command", "filePath", "path", "workdir", "name"} {
		if raw, ok := args[key]; ok {
			value := strings.TrimSpace(fmt.Sprint(raw))
			if value == "" {
				continue
			}
			lower := strings.ToLower(value)
			for _, secretWord := range []string{"authorization:", "bearer ", "access_token", "api_key", "apikey", "password", "secret"} {
				if strings.Contains(lower, secretWord) {
					return "🔧 " + name + ": <lệnh có dữ liệu nhạy cảm — đã ẩn>"
				}
			}
			if len(value) > 420 {
				value = value[:420] + "…"
			}
			return "🔧 " + name + ": `" + strings.ReplaceAll(value, "`", "'") + "`"
		}
	}
	if raw, ok := args["patchText"]; ok {
		patch := fmt.Sprint(raw)
		targets := make([]string, 0, 4)
		for _, line := range strings.Split(patch, "\n") {
			line = strings.TrimSpace(line)
			for _, prefix := range []string{"*** Add File:", "*** Update File:", "*** Delete File:"} {
				if strings.HasPrefix(line, prefix) {
					targets = append(targets, strings.TrimSpace(strings.TrimPrefix(line, prefix)))
				}
			}
			if len(targets) >= 4 {
				break
			}
		}
		if len(targets) > 0 {
			return "🔧 " + name + ": " + strings.Join(targets, ", ")
		}
	}
	return "🔧 " + name + ": đang thực thi"
}
func (h *ChatHandler) ChatGPTConversation(c *gin.Context) {
	var original_request chatgpt_types.ChatGPTRequest
	if err := c.BindJSON(&original_request); err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "Request must be proper JSON",
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    err.Error(),
		}})
		return
	}
	if len(original_request.Messages) > 0 && original_request.Messages[0].Author.Role == "" {
		original_request.Messages[0].Author.Role = "user"
	}

	account, _, err := resolveAccount(c, h.accountPool, h.cfg, false)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "authorization_error",
			"param":   "Authorization",
			"code":    400,
		}})
		return
	}
	if account == nil || account.Token == "" || !account.Type.Satisfies(accounts.CapChat) {
		c.JSON(400, gin.H{"error": "Not Account Found."})
		return
	}

	// 使用 account 绑定的 Client（有指纹 + 代理）；不存在则新建
	var convClient *bogdanfinn.TlsClient
	if c, ok := account.Client.(*bogdanfinn.TlsClient); ok && c != nil {
		convClient = c
	} else {
		convClient = bogdanfinn.NewStdClient()
		if account.Proxy != "" {
			convClient.SetProxy(account.Proxy)
		}
	}
	turnStile, status, err := chatgpt.InitSentinel(convClient, account, account.Proxy, 0)
	if err != nil {
		if status == http.StatusUnauthorized {
			h.accountPool.ReportFailure(account)
		}
		c.JSON(status, gin.H{
			"message": err.Error(),
			"type":    "InitTurnStile_request_error",
			"param":   err,
			"code":    status,
		})
		return
	}

	response, err := chatgpt.POSTconversation(convClient, original_request, account, turnStile, account.Proxy)
	if err != nil {
		c.JSON(500, gin.H{"error": "error sending request"})
		return
	}
	defer response.Body.Close()

	if chatgpt.Handle_request_error(c, response) {
		return
	}

	c.Header("Content-Type", response.Header.Get("Content-Type"))
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "" {
		c.Header("Cache-Control", cacheControl)
	}

	if _, err := io.Copy(c.Writer, response.Body); err != nil {
		c.JSON(500, gin.H{"error": "Error sending response"})
	}
}
