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

	systemPrompt = `你是 AI 助手，一个有帮助、友好的智能助理。请用简洁清晰的中文回答问题。`
}

// callDeepSeekStream 调用 DeepSeek API 进行流式对话。
// - history: 对话历史（不含 system prompt）
// - enableSearch: 是否传递搜索参数给 DeepSeek（备用，实际搜索由 search.go 处理）
// - sendChunk: 每个文本增量调用的回调
// - onSearchResults: DeepSeek 返回搜索结果时的回调（备用）
func callDeepSeekStream(history []ChatMessage, enableSearch bool, sendChunk func(string) error, onSearchResults func([]SearchResult)) error {
	if deepseekAPIKey == "" {
		sendChunk("（AI 服务未配置 — 请设置 DEEPSEEK_API_KEY）")
		return nil
	}

	// 组装请求：system prompt + 对话历史
	messages := append([]ChatMessage{{Role: "system", Content: systemPrompt}}, history...)
	reqBody := ChatCompletionRequest{
		Model:        deepseekModel,
		Messages:     messages,
		Stream:       true,
		EnableSearch: enableSearch,
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

	// 连接 MySQL
	db, err := initMySQL()
	if err != nil {
		log.Fatalf("MySQL 初始化失败: %v", err)
	}
	defer db.Close()

	// 组装应用
	app := &App{
		hub:           newHub(),
		users:         newUserStore(),
		sessions:      newSessionStore(),
		conversations: newConversationStore(db),
		db:            db,
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
