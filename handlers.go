package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// ═══════════════════════════════════════════════════════════════
// 中间件
// ═══════════════════════════════════════════════════════════════

// authMiddleware 是认证中间件。
// - 页面请求：未认证返回 302 重定向到 /login?expired=1
// - API/WS 请求：未认证返回 401 JSON
// - 已认证时自动刷新会话活跃时间
func (app *App) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := app.getSession(r)
		if session == nil || session.IsIdleExpired() || session.IsExpired() {
			if session != nil {
				app.sessions.Delete(app.getSessionToken(r))
			}
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws") {
				http.Error(w, `{"error":"unauthorized","reason":"session_expired"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?expired=1", http.StatusFound)
			return
		}
		// 刷新最后活跃时间
		app.sessions.Touch(app.getSessionToken(r))
		next(w, r)
	}
}

// getSessionToken 从 Cookie 中提取 session_token。
func (app *App) getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return ""
	}
	return cookie.Value
}

// getSession 根据请求中的 Cookie 获取会话对象。
func (app *App) getSession(r *http.Request) *Session {
	token := app.getSessionToken(r)
	if token == "" {
		return nil
	}
	return app.sessions.Get(token)
}

// getSessionUsername 快捷获取当前请求的用户名。
func (app *App) getSessionUsername(r *http.Request) string {
	session := app.getSession(r)
	if session == nil {
		return ""
	}
	return session.Username
}

// ═══════════════════════════════════════════════════════════════
// 登录 / 登出 / 会话状态
// ═══════════════════════════════════════════════════════════════

// handleLogin 处理登录页面（GET）和登录请求（POST）。
func (app *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	// GET —— 返回登录页面
	if r.Method == http.MethodGet {
		http.ServeFile(w, r, "./static/login.html")
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// POST —— 处理登录
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, LoginResponse{Success: false, Message: "请求格式错误"})
		return
	}

	if !app.users.Validate(req.Username, req.Password) {
		writeJSON(w, http.StatusUnauthorized, LoginResponse{Success: false, Message: "用户名或密码错误"})
		return
	}

	// 创建会话，设置 Cookie
	token := app.sessions.Create(req.Username)
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400, // 24 小时
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, LoginResponse{Success: true, Token: token, Message: "登录成功"})
	log.Printf("用户 %s 已登录", req.Username)
}

// handleLogout 处理登出请求，删除会话并清除 Cookie。
func (app *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cookie, err := r.Cookie("session_token")
	if err == nil {
		app.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1, // 立即过期
	})
	writeJSON(w, http.StatusOK, LoginResponse{Success: true, Message: "已退出登录"})
}

// handleSession 返回当前会话状态和空闲剩余秒数。
func (app *App) handleSession(w http.ResponseWriter, r *http.Request) {
	session := app.getSession(r)
	if session != nil && !session.IsIdleExpired() && !session.IsExpired() {
		remaining := int((sessionIdleTimeout - time.Since(session.LastActivity)).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		writeJSON(w, http.StatusOK, SessionResponse{
			Authenticated: true,
			Username:      session.Username,
			RemainingSecs: remaining,
		})
	} else {
		writeJSON(w, http.StatusOK, SessionResponse{Authenticated: false})
	}
}

// ═══════════════════════════════════════════════════════════════
// 会话（对话）管理 API
// ═══════════════════════════════════════════════════════════════

// handleListConversations 列出当前用户的所有会话。
func (app *App) handleListConversations(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}

	list, err := app.listConversations(username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateConversation 创建新会话（最多 3 个）。
func (app *App) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}

	var req struct {
		Title string `json:"title"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	conv, err := app.createConversation(username, req.Title)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, conv)
	log.Printf("[%s] 创建会话 %d：%s", username, conv.ID, conv.Title)
}

// handleConversationMessages 加载指定会话的历史消息。
func (app *App) handleConversationMessages(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}

	idStr := r.PathValue("id")
	convID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的会话 ID"})
		return
	}

	msgs, err := app.loadMessages(convID, username)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if msgs == nil {
		msgs = []ChatMessage{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// handleDeleteConversation 删除会话及其所有消息。
func (app *App) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}

	idStr := r.PathValue("id")
	convID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的会话 ID"})
		return
	}

	if err := app.deleteConversation(convID, username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	log.Printf("[%s] 删除会话 %d", username, convID)
}

// handleRenameConversation 重命名会话。
func (app *App) handleRenameConversation(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}

	idStr := r.PathValue("id")
	convID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的会话 ID"})
		return
	}

	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "标题不能为空"})
		return
	}

	if err := app.renameConversation(convID, username, req.Title); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "renamed"})
}

// ═══════════════════════════════════════════════════════════════
// 聊天页面 & WebSocket
// ═══════════════════════════════════════════════════════════════

// handleChat 返回聊天页面（认证由 authMiddleware 保证）。
func (app *App) handleChat(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

// handleWS 处理 WebSocket 连接。
// 升级后启动两个 goroutine：write pump（发送）和 read pump（接收+AI处理）。
func (app *App) handleWS(w http.ResponseWriter, r *http.Request) {
	session := app.getSession(r)
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 升级 HTTP 为 WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket 升级失败:", err)
		return
	}

	client := &Client{
		ID:       r.RemoteAddr,
		Username: session.Username,
		Conn:     conn,
		Send:     make(chan []byte, 256),
	}

	app.hub.register <- client

	// ── write pump：将 Send 通道中的消息写入 WebSocket ──────
	go func() {
		defer conn.Close()
		for message := range client.Send {
			if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		}
	}()

	// ── read pump：接收消息 → 搜索 → AI 流式回复 ────────────
	go func() {
		defer func() {
			app.hub.unregister <- client
			conn.Close()
		}()

		// Ping 保活：每 30 秒发送 ping，防止代理/NAT 关闭空闲连接
		pingDone := make(chan struct{})
		defer close(pingDone)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-pingDone:
					return
				case <-ticker.C:
					conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
						return
					}
				}
			}
		}()

		for {
			_, rawMsg, err := conn.ReadMessage()
			if err != nil {
				break
			}

			var msg Message
			if err := json.Unmarshal(rawMsg, &msg); err != nil {
				continue
			}

			// 只处理 message 类型
			if msg.Type != "message" || msg.Content == "" {
				continue
			}

			convID := msg.ConversationID
			if convID == 0 {
				errMsg, _ := json.Marshal(Message{
					Type:    "error",
					Content: "请先选择或创建一个会话",
					Sender:  "server",
				})
				client.Send <- errMsg
				continue
			}

			// 1. 保存用户消息到数据库
			if err := app.conversations.AddMessage(convID, "user", msg.Content); err != nil {
				log.Printf("保存用户消息失败: %v", err)
			}

			// 2. 发送 stream_start 信号给前端
			startMsg, _ := json.Marshal(Message{
				Type:           "stream_start",
				Sender:         "server",
				Username:       "AI",
				ConversationID: convID,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
			})
			client.Send <- startMsg

			// 3. 加载对话历史
			history, err := app.conversations.GetHistory(convID)
			if err != nil {
				log.Printf("加载会话 %d 历史失败: %v", convID, err)
				history = nil
			}

			// 4. 如果启用联网搜索，先搜索再注入上下文
			if msg.EnableSearch {
				searchResults := searchWeb(msg.Content)
				if len(searchResults) > 0 {
					// 将搜索结果发送给前端展示
					searchMsg, _ := json.Marshal(Message{
						Type:           "search_results",
						Content:        toSearchResultsJSON(searchResults),
						Sender:         "server",
						Username:       "AI",
						ConversationID: convID,
						Timestamp:      time.Now().UTC().Format(time.RFC3339),
					})
					client.Send <- searchMsg

					// 将搜索结果注入对话上下文
					history = buildSearchContext(history, searchResults)
					log.Printf("[%s] 搜索: %d 条结果 ← %q", client.Username, len(searchResults), truncate(msg.Content, 40))
				}
			}

			// 5. 调用 DeepSeek API 流式生成回复
			var aiContent string
			enableSearch := msg.EnableSearch
			err = callDeepSeekStream(history, enableSearch,
				// sendChunk: 每个文本块推送给前端
				func(chunk string) error {
					aiContent += chunk
					chunkMsg, _ := json.Marshal(Message{
						Type:           "stream_chunk",
						Content:        chunk,
						Sender:         "server",
						Username:       "AI",
						ConversationID: convID,
						Timestamp:      time.Now().UTC().Format(time.RFC3339),
					})
					select {
					case client.Send <- chunkMsg:
					default:
					}
					return nil
				},
				// onSearchResults: DeepSeek 原生搜索结果（备用通道）
				func(results []SearchResult) {
					searchMsg, _ := json.Marshal(Message{
						Type:           "search_results",
						Content:        toSearchResultsJSON(results),
						Sender:         "server",
						Username:       "AI",
						ConversationID: convID,
						Timestamp:      time.Now().UTC().Format(time.RFC3339),
					})
					select {
					case client.Send <- searchMsg:
					default:
					}
				})

			if err != nil {
				log.Printf("DeepSeek API 错误 [%s]: %v", client.Username, err)
				errMsg, _ := json.Marshal(Message{
					Type:           "stream_chunk",
					Content:        fmt.Sprintf("抱歉，AI 服务暂时不可用：%v", err),
					Sender:         "server",
					Username:       "AI",
					ConversationID: convID,
					Timestamp:      time.Now().UTC().Format(time.RFC3339),
				})
				client.Send <- errMsg
				aiContent = fmt.Sprintf("[错误] %v", err)
			}

			// 6. 保存 AI 回复到数据库
			if aiContent != "" {
				if err := app.conversations.AddMessage(convID, "assistant", aiContent); err != nil {
					log.Printf("保存 AI 回复失败: %v", err)
				}
			}

			// 7. 发送 stream_end 结束信号
			endMsg, _ := json.Marshal(Message{
				Type:           "stream_end",
				Content:        aiContent,
				Sender:         "server",
				Username:       "AI",
				ConversationID: convID,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
			})
			client.Send <- endMsg
		}
	}()
}

// ═══════════════════════════════════════════════════════════════
// 静态文件服务
// ═══════════════════════════════════════════════════════════════

// serveStatic 提供静态文件服务。
// login.css / login.js / health 为公开访问，其余需登录。
func (app *App) serveStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 公开文件
	if path == "/login.css" || path == "/login.js" || path == "/health" {
		http.ServeFile(w, r, "./static"+path)
		return
	}

	// 受保护文件
	session := app.getSession(r)
	if session == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	http.ServeFile(w, r, "./static"+path)
}

// ═══════════════════════════════════════════════════════════════
// 工具函数
// ═══════════════════════════════════════════════════════════════

// writeJSON 写入 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
