package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ═══════════════════════════════════════════════════════════════
// DeepSeek AI 客户端
// ═══════════════════════════════════════════════════════════════

// initDeepSeek 从环境变量加载 DeepSeek 配置。
func initDeepSeek() {
	deepseekAPIKey = os.Getenv("DEEPSEEK_API_KEY")
	if deepseekAPIKey == "" {
		log.Println("⚠️  DEEPSEEK_API_KEY 未设置，AI 将不可用")
	}

	deepseekBaseURL = os.Getenv("DEEPSEEK_BASE_URL")
	if deepseekBaseURL == "" {
		deepseekBaseURL = "https://api.deepseek.com"
	}

	deepseekModel = os.Getenv("DEEPSEEK_MODEL")
	if deepseekModel == "" {
		deepseekModel = "deepseek-chat"
	}

	systemPrompt = `你是 AI 助手，有帮助、友好的智能助理。用简洁中文回答。

你有联网搜索能力。当系统提示中包含搜索结果时，直接引用并标来源 [1][2]。
不要建议用户"开启联网搜索"——你就是联网的。
被问"你能联网吗"直接说"能"。
不要复述搜索结果原文，直接基于它们回答。`
}

// initOpenClaw 从环境变量加载 OpenClaw 网关配置。
// 当 OPENCLAW_ENABLED=true 时，AI 请求将通过 OpenClaw 网关路由，
// 而非直接调用 DeepSeek API。
func initOpenClaw() {
	openclawEnabled = os.Getenv("OPENCLAW_ENABLED") == "true"
	if !openclawEnabled {
		log.Println("OpenClaw 未启用（OPENCLAW_ENABLED != true），使用直连 DeepSeek")
		return
	}

	openclawBaseURL = os.Getenv("OPENCLAW_BASE_URL")
	if openclawBaseURL == "" {
		openclawBaseURL = "http://127.0.0.1:18789"
	}

	openclawAuthToken = os.Getenv("OPENCLAW_AUTH_TOKEN")
	if openclawAuthToken == "" {
		log.Println("⚠️  OPENCLAW_ENABLED=true 但 OPENCLAW_AUTH_TOKEN 未设置，将回退到直连 DeepSeek")
		openclawEnabled = false
		return
	}

	log.Println("OpenClaw 集成已启用:", openclawBaseURL)
}

// callOpenClawStream 通过 OpenClaw 网关调用 AI 进行流式对话。
// API 兼容 OpenAI 格式，与 callDeepSeekStream 保持相同的回调语义。
// - username + conversationID: 用于 OpenClaw session 一致性（同一会话复用相同 session）
// - userMessage: 当前用户消息正文
// OpenClaw 根据 agents.defaults.model.primary 配置路由到后端模型。
func callOpenClawStream(username string, conversationID int64, app *App, userMessage string, history []ChatMessage, sendChunk func(string) error) error {
	if !openclawEnabled || openclawAuthToken == "" {
		// 回退到直连 DeepSeek（需要完整 history）
		return callDeepSeekStream(history, nil, sendChunk, nil)
	}

	// 组装请求：system prompt + 仅当前消息
	// OpenClaw 通过 session 自行维护上下文，无需每次发送完整历史
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMessage},
	}
	reqBody := ChatCompletionRequest{
		Model:    "openclaw/default", // OpenClaw 根据配置路由到实际模型
		Messages: messages,
		Stream:   true,
		User:     fmt.Sprintf("%s-conv-%d", username, conversationID), // 稳定 session 标识
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequest("POST", openclawBaseURL+"/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+openclawAuthToken)
	// 透传 DeepSeek 模型名，供 OpenClaw 精确路由
	httpReq.Header.Set("x-openclaw-model", "deepseek/deepseek-chat")

	// 如果有已存储的 session key，传递以复用 session
	if sessionKey := app.openclawSessions.Get(conversationID); sessionKey != "" {
		httpReq.Header.Set("x-openclaw-session-key", sessionKey)
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("OpenClaw API 调用失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OpenClaw API 错误 %d: %s", resp.StatusCode, string(body))
	}

	// 存储 OpenClaw 返回的 session key（后续消息复用）
	if sk := resp.Header.Get("x-openclaw-session-key"); sk != "" {
		app.openclawSessions.Set(conversationID, sk)
	}

	// 解析 SSE 流（与 DeepSeek 相同的格式）
	reader := bufio.NewReader(resp.Body)
	fullContent := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("读取流失败: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// 提取文本增量
		for _, choice := range chunk.Choices {
			content := choice.Delta.Content
			if content != "" {
				fullContent += content
				if err := sendChunk(content); err != nil {
					return fmt.Errorf("发送文本块失败: %w", err)
				}
			}
		}
	}

	if fullContent == "" {
		sendChunk("（AI 返回了空响应，请稍后重试）")
	}
	return nil
}

// closeOpenClawSession 关闭指定会话对应的 OpenClaw session。
// 在用户删除会话时调用，释放 OpenClaw 端资源。
func closeOpenClawSession(app *App, conversationID int64) {
	if !openclawEnabled {
		return
	}

	sessionKey := app.openclawSessions.Get(conversationID)
	if sessionKey == "" {
		return
	}

	req, _ := http.NewRequest("DELETE", openclawBaseURL+"/v1/sessions/"+sessionKey, nil)
	req.Header.Set("Authorization", "Bearer "+openclawAuthToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("关闭 OpenClaw session 失败 [conv:%d]: %v", conversationID, err)
		return
	}
	defer resp.Body.Close()

	app.openclawSessions.Delete(conversationID)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		log.Printf("OpenClaw session 已关闭 [conv:%d, session:%s]", conversationID, sessionKey)
	} else {
		log.Printf("OpenClaw session 关闭返回 %d [conv:%d]", resp.StatusCode, conversationID)
	}
}

// callDeepSeekStream 调用 DeepSeek API 进行流式对话。
// - history: 对话历史（不含 system prompt）
// - searchResults: 搜索结果（nil 表示未启用搜索），合并到 system prompt 中
// - sendChunk: 每个文本增量调用的回调
// - onSearchResults: DeepSeek 返回搜索结果时的回调（备用）
func callDeepSeekStream(history []ChatMessage, searchResults []SearchResult, sendChunk func(string) error, onSearchResults func([]SearchResult)) error {
	if deepseekAPIKey == "" {
		sendChunk("（AI 服务未配置 — 请设置 DEEPSEEK_API_KEY）")
		return nil
	}

	// 构建 system prompt：基础 prompt + 搜索结果（如有）
	prompt := systemPrompt
	if len(searchResults) > 0 {
		prompt = buildSearchPrompt(systemPrompt, searchResults)
	} else if searchResults != nil {
		// 搜索已执行但无结果：告诉 AI 诚实承认，不要编造
		prompt = systemPrompt + "\n\n---\n你刚才尝试联网搜索但未获取到任何结果。" +
			"请如实告知用户搜索未找到相关信息，不要编造具体数据或日期。" +
			"可以建议用户换关键词重试，或调整搜索范围。"
	}

	// 组装请求：system prompt + 对话历史
	messages := append([]ChatMessage{{Role: "system", Content: prompt}}, history...)
	reqBody := ChatCompletionRequest{
		Model:    deepseekModel,
		Messages: messages,
		Stream:   true,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}

	httpReq, err := http.NewRequest("POST", deepseekBaseURL+"/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+deepseekAPIKey)

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("API 调用失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API 错误 %d: %s", resp.StatusCode, string(body))
	}

	// 解析 SSE 流
	reader := bufio.NewReader(resp.Body)
	fullContent := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("读取流失败: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// 处理搜索结果（DeepSeek 原生搜索通道）
		if len(chunk.SearchResults) > 0 && onSearchResults != nil {
			onSearchResults(chunk.SearchResults)
		}

		// 提取文本增量
		for _, choice := range chunk.Choices {
			content := choice.Delta.Content
			if content != "" {
				fullContent += content
				if err := sendChunk(content); err != nil {
					return fmt.Errorf("发送文本块失败: %w", err)
				}
			}
		}
	}

	if fullContent == "" {
		sendChunk("（AI 返回了空响应，请稍后重试）")
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════
// MySQL 初始化
// ═══════════════════════════════════════════════════════════════

// initMySQL 连接 MySQL 并验证连通性。
// DSN 通过 MYSQL_DSN 环境变量注入。
func initMySQL() (*sql.DB, error) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("MYSQL_DSN 环境变量未设置")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 连接失败: %w", err)
	}

	// 配置连接池（适配低内存服务器）
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("MySQL 连接测试失败: %w", err)
	}

	log.Println("MySQL 已连接")
	return db, nil
}

// ═══════════════════════════════════════════════════════════════
// 入口
// ═══════════════════════════════════════════════════════════════

func main() {
	initDeepSeek()
	initOpenClaw()

	// 连接 MySQL
	db, err := initMySQL()
	if err != nil {
		log.Fatalf("MySQL 初始化失败: %v", err)
	}
	defer db.Close()

	// 组装应用
	app := &App{
		hub:              newHub(),
		users:            newUserStore(),
		sessions:         newSessionStore(),
		conversations:    newConversationStore(db),
		openclawSessions: newOpenClawSessionStore(),
		db:               db,
	}
	go app.hub.run()

	// 后台每 5 分钟清理过期会话
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			app.sessions.Cleanup()
		}
	}()

	// ── 注册路由 ──────────────────────────────────────────────

	// 公开路由（无需登录）
	http.HandleFunc("/login", app.handleLogin)
	http.HandleFunc("/login.css", app.serveStatic)
	http.HandleFunc("/login.js", app.serveStatic)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
	})

	// 认证 API（内部处理认证逻辑）
	http.HandleFunc("/api/login", app.handleLogin)
	http.HandleFunc("/api/logout", app.handleLogout)
	http.HandleFunc("/api/session", app.handleSession)

	// 会话管理 API（需登录）
	http.HandleFunc("GET /api/conversations", app.authMiddleware(app.handleListConversations))
	http.HandleFunc("POST /api/conversations", app.authMiddleware(app.handleCreateConversation))
	http.HandleFunc("GET /api/conversations/{id}/messages", app.authMiddleware(app.handleConversationMessages))
	http.HandleFunc("DELETE /api/conversations/{id}", app.authMiddleware(app.handleDeleteConversation))
	http.HandleFunc("PUT /api/conversations/{id}", app.authMiddleware(app.handleRenameConversation))

	// 受保护路由
	http.HandleFunc("/ws", app.authMiddleware(app.handleWS))
	http.HandleFunc("/", app.authMiddleware(app.handleChat))
	http.HandleFunc("/style.css", app.serveStatic)
	http.HandleFunc("/app.js", app.serveStatic)

	// ── 启动 ──────────────────────────────────────────────────

	addr := ":8080"
	log.Printf("🚀 Chat Assistant 启动于 %s", addr)
	log.Printf("   登录页面: /login")
	log.Printf("   聊天页面: /")
	log.Printf("   WebSocket: /ws")
	log.Printf("   MySQL:     已连接")
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("服务启动失败:", err)
	}
}
