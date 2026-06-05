package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

// ─── Data Types ────────────────────────────────────────────────

type Message struct {
	Type           string `json:"type"`
	Content        string `json:"content,omitempty"`
	Sender         string `json:"sender,omitempty"`
	Username       string `json:"username,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"`
	ConversationID int64  `json:"conversation_id,omitempty"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Token   string `json:"token,omitempty"`
}

type SessionResponse struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
	RemainingSecs int    `json:"remaining_secs,omitempty"`
}

const (
	sessionMaxAge      = 24 * time.Hour
	sessionIdleTimeout = 10 * time.Minute
	maxConversations   = 3
)

// ConversationInfo is returned in list API (lightweight, no messages)
type ConversationInfo struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
}

// ─── DeepSeek API Types ─────────────────────────────────────────

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ChatCompletionChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// ─── Conversation Store (MySQL + Memory Cache) ──────────────────

type ConversationStore struct {
	mu       sync.RWMutex
	cache    map[int64][]ChatMessage
	maxTurns int
	db       *sql.DB
}

func newConversationStore(db *sql.DB) *ConversationStore {
	return &ConversationStore{
		cache:    make(map[int64][]ChatMessage),
		maxTurns: 20,
		db:       db,
	}
}

func (cs *ConversationStore) AddMessage(convID int64, role, content string) error {
	_, err := cs.db.Exec(
		"INSERT INTO messages (conversation_id, role, content) VALUES (?, ?, ?)",
		convID, role, content,
	)
	if err != nil {
		return fmt.Errorf("save message: %w", err)
	}

	cs.mu.Lock()
	msgs := cs.cache[convID]
	msgs = append(msgs, ChatMessage{Role: role, Content: content})
	if len(msgs) > cs.maxTurns*2+1 {
		cut := len(msgs) - cs.maxTurns*2
		if cut < 1 {
			cut = 1
		}
		msgs = append(msgs[:1], msgs[cut:]...)
	}
	cs.cache[convID] = msgs
	cs.mu.Unlock()

	cs.db.Exec("UPDATE conversations SET updated_at = NOW() WHERE id = ?", convID)
	return nil
}

func (cs *ConversationStore) GetHistory(convID int64) ([]ChatMessage, error) {
	cs.mu.RLock()
	cached, ok := cs.cache[convID]
	cs.mu.RUnlock()
	if ok {
		return cached, nil
	}

	rows, err := cs.db.Query(
		`SELECT role, content FROM messages
		 WHERE conversation_id = ?
		 ORDER BY id DESC LIMIT ?`,
		convID, cs.maxTurns*2,
	)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()

	var msgs []ChatMessage
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			continue
		}
		msgs = append(msgs, ChatMessage{Role: role, Content: content})
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	cs.mu.Lock()
	cs.cache[convID] = msgs
	cs.mu.Unlock()
	return msgs, nil
}

func (cs *ConversationStore) ClearCache(convID int64) {
	cs.mu.Lock()
	delete(cs.cache, convID)
	cs.mu.Unlock()
}

// ─── Conversation DB Helpers ──────────────────────────────────────

func (app *App) listConversations(username string) ([]ConversationInfo, error) {
	rows, err := app.db.Query(
		`SELECT c.id, c.title, c.created_at, c.updated_at,
		        COALESCE((SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id), 0) as msg_count
		 FROM conversations c
		 WHERE c.username = ?
		 ORDER BY c.updated_at DESC`,
		username,
	)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var list []ConversationInfo
	for rows.Next() {
		var ci ConversationInfo
		if err := rows.Scan(&ci.ID, &ci.Title, &ci.CreatedAt, &ci.UpdatedAt, &ci.MessageCount); err != nil {
			continue
		}
		list = append(list, ci)
	}
	if list == nil {
		list = []ConversationInfo{}
	}
	return list, nil
}

func (app *App) createConversation(username, title string) (*ConversationInfo, error) {
	var count int
	if err := app.db.QueryRow(
		"SELECT COUNT(*) FROM conversations WHERE username = ?", username,
	).Scan(&count); err != nil {
		return nil, fmt.Errorf("count conversations: %w", err)
	}
	if count >= maxConversations {
		return nil, fmt.Errorf("已达到最大会话数上限（%d 个）", maxConversations)
	}

	if title == "" {
		title = "新对话"
	}

	res, err := app.db.Exec(
		"INSERT INTO conversations (username, title) VALUES (?, ?)",
		username, title,
	)
	if err != nil {
		return nil, fmt.Errorf("create conversation: %w", err)
	}

	id, _ := res.LastInsertId()
	now := time.Now()
	return &ConversationInfo{
		ID:        id,
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (app *App) deleteConversation(id int64, username string) error {
	var owner string
	if err := app.db.QueryRow(
		"SELECT username FROM conversations WHERE id = ?", id,
	).Scan(&owner); err != nil {
		return fmt.Errorf("conversation not found: %w", err)
	}
	if owner != username {
		return fmt.Errorf("permission denied")
	}

	if _, err := app.db.Exec("DELETE FROM conversations WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}

	app.conversations.ClearCache(id)
	return nil
}

func (app *App) renameConversation(id int64, username, title string) error {
	var owner string
	if err := app.db.QueryRow(
		"SELECT username FROM conversations WHERE id = ?", id,
	).Scan(&owner); err != nil {
		return fmt.Errorf("conversation not found: %w", err)
	}
	if owner != username {
		return fmt.Errorf("permission denied")
	}

	if _, err := app.db.Exec(
		"UPDATE conversations SET title = ? WHERE id = ?", title, id,
	); err != nil {
		return fmt.Errorf("rename conversation: %w", err)
	}
	return nil
}

func (app *App) loadMessages(convID int64, username string) ([]ChatMessage, error) {
	var owner string
	if err := app.db.QueryRow(
		"SELECT username FROM conversations WHERE id = ?", convID,
	).Scan(&owner); err != nil {
		return nil, fmt.Errorf("conversation not found: %w", err)
	}
	if owner != username {
		return nil, fmt.Errorf("permission denied")
	}

	return app.conversations.GetHistory(convID)
}

// ─── DeepSeek Client ────────────────────────────────────────────

var (
	deepseekAPIKey  string
	deepseekBaseURL string
	deepseekModel   string
	systemPrompt    string
)

func initDeepSeek() {
	deepseekAPIKey = os.Getenv("DEEPSEEK_API_KEY")
	if deepseekAPIKey == "" {
		log.Println("WARNING: DEEPSEEK_API_KEY not set, AI replies will fall back to echo")
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

func callDeepSeekStream(history []ChatMessage, sendChunk func(string) error) error {
	if deepseekAPIKey == "" {
		sendChunk("（AI 服务未配置 — 请设置 DEEPSEEK_API_KEY）")
		return nil
	}

	messages := append([]ChatMessage{{Role: "system", Content: systemPrompt}}, history...)
	reqBody := ChatCompletionRequest{
		Model:    deepseekModel,
		Messages: messages,
		Stream:   true,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", deepseekBaseURL+"/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+deepseekAPIKey)

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	fullContent := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read stream: %w", err)
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

		for _, choice := range chunk.Choices {
			content := choice.Delta.Content
			if content != "" {
				fullContent += content
				if err := sendChunk(content); err != nil {
					return fmt.Errorf("send chunk: %w", err)
				}
			}
		}
	}

	if fullContent == "" {
		sendChunk("（AI 返回了空响应，请稍后重试）")
	}
	return nil
}

// ─── In-Memory User Store ──────────────────────────────────────

type UserStore struct {
	mu    sync.RWMutex
	users map[string]string
}

func newUserStore() *UserStore {
	us := &UserStore{users: make(map[string]string)}
	us.users["Klein4062"] = "$2b$12$c.8cW/ZBKNpbcpfOYNg3E.5.yMdFf84.LmXd.qJ1WPVvrHFQvpxg6"
	return us
}

func (us *UserStore) Validate(username, password string) bool {
	us.mu.RLock()
	hash, ok := us.users[username]
	us.mu.RUnlock()
	if !ok {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ─── Session Store ─────────────────────────────────────────────

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

type Session struct {
	Username     string
	CreatedAt    time.Time
	LastActivity time.Time
}

func (s *Session) IsIdleExpired() bool {
	return time.Since(s.LastActivity) > sessionIdleTimeout
}

func (s *Session) IsExpired() bool {
	return time.Since(s.CreatedAt) > sessionMaxAge
}

func newSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]*Session)}
}

func (ss *SessionStore) Create(username string) string {
	token := generateToken()
	now := time.Now()
	ss.mu.Lock()
	ss.sessions[token] = &Session{Username: username, CreatedAt: now, LastActivity: now}
	ss.mu.Unlock()
	return token
}

func (ss *SessionStore) Get(token string) *Session {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.sessions[token]
}

func (ss *SessionStore) Touch(token string) {
	ss.mu.Lock()
	if s, ok := ss.sessions[token]; ok {
		s.LastActivity = time.Now()
	}
	ss.mu.Unlock()
}

func (ss *SessionStore) Delete(token string) {
	ss.mu.Lock()
	delete(ss.sessions, token)
	ss.mu.Unlock()
}

func (ss *SessionStore) Cleanup() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for token, s := range ss.sessions {
		if s.IsIdleExpired() || s.IsExpired() {
			delete(ss.sessions, token)
			log.Printf("Session expired for %s (idle: %v)", s.Username, time.Since(s.LastActivity).Round(time.Second))
		}
	}
}

func generateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ─── WebSocket Hub ─────────────────────────────────────────────

type Client struct {
	ID       string
	Username string
	Conn     *websocket.Conn
	Send     chan []byte
}

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			count := len(h.clients)
			h.mu.Unlock()
			log.Printf("[%s] connected (total: %d)", client.Username, count)

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.Send)
			}
			count := len(h.clients)
			h.mu.Unlock()
			log.Printf("[%s] disconnected (total: %d)", client.Username, count)

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.Send <- message:
				default:
					close(client.Send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

// ─── App State ─────────────────────────────────────────────────

type App struct {
	hub           *Hub
	users         *UserStore
	sessions      *SessionStore
	conversations *ConversationStore
	db            *sql.DB
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// ─── Middleware ─────────────────────────────────────────────────

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
		app.sessions.Touch(app.getSessionToken(r))
		next(w, r)
	}
}

func (app *App) getSessionToken(r *http.Request) string {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (app *App) getSession(r *http.Request) *Session {
	token := app.getSessionToken(r)
	if token == "" {
		return nil
	}
	return app.sessions.Get(token)
}

func (app *App) getSessionUsername(r *http.Request) string {
	session := app.getSession(r)
	if session == nil {
		return ""
	}
	return session.Username
}

// ─── Handlers ──────────────────────────────────────────────────

func (app *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		http.ServeFile(w, r, "./static/login.html")
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, LoginResponse{Success: false, Message: "Invalid request"})
		return
	}

	if !app.users.Validate(req.Username, req.Password) {
		writeJSON(w, http.StatusUnauthorized, LoginResponse{Success: false, Message: "用户名或密码错误"})
		return
	}

	token := app.sessions.Create(req.Username)
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, LoginResponse{Success: true, Token: token, Message: "登录成功"})
	log.Printf("User %s logged in", req.Username)
}

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
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, LoginResponse{Success: true, Message: "已退出登录"})
}

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

// ─── Conversation API Handlers ──────────────────────────────────

func (app *App) handleListConversations(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	list, err := app.listConversations(username)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (app *App) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
	log.Printf("[%s] created conversation %d: %s", username, conv.ID, conv.Title)
}

func (app *App) handleConversationMessages(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	idStr := r.PathValue("id")
	convID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation id"})
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

func (app *App) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	idStr := r.PathValue("id")
	convID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation id"})
		return
	}

	if err := app.deleteConversation(convID, username); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	log.Printf("[%s] deleted conversation %d", username, convID)
}

func (app *App) handleRenameConversation(w http.ResponseWriter, r *http.Request) {
	username := app.getSessionUsername(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	idStr := r.PathValue("id")
	convID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid conversation id"})
		return
	}

	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}

	if err := app.renameConversation(convID, username, req.Title); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "renamed"})
}

func (app *App) handleChat(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

func (app *App) handleWS(w http.ResponseWriter, r *http.Request) {
	session := app.getSession(r)
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}

	client := &Client{
		ID:       r.RemoteAddr,
		Username: session.Username,
		Conn:     conn,
		Send:     make(chan []byte, 256),
	}

	app.hub.register <- client

	// Write pump
	go func() {
		defer conn.Close()
		for message := range client.Send {
			if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		}
	}()

	// Read pump
	go func() {
		defer func() {
			app.hub.unregister <- client
			conn.Close()
		}()

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

			if msg.Type != "message" || msg.Content == "" {
				continue
			}

			convID := msg.ConversationID
			if convID == 0 {
				// Client must specify a conversation
				errMsg, _ := json.Marshal(Message{
					Type:    "error",
					Content: "请先选择或创建一个会话",
					Sender:  "server",
				})
				client.Send <- errMsg
				continue
			}

			// Save user message to DB
			if err := app.conversations.AddMessage(convID, "user", msg.Content); err != nil {
				log.Printf("Failed to save user message: %v", err)
			}

			// Send stream_start
			startMsg, _ := json.Marshal(Message{
				Type:           "stream_start",
				Sender:         "server",
				Username:       "AI",
				ConversationID: convID,
				Timestamp:      time.Now().UTC().Format(time.RFC3339),
			})
			client.Send <- startMsg

			// Load history for AI context
			history, err := app.conversations.GetHistory(convID)
			if err != nil {
				log.Printf("Failed to load history for conv %d: %v", convID, err)
				history = nil
			}

			var aiContent string
			err = callDeepSeekStream(history, func(chunk string) error {
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
			})

			if err != nil {
				log.Printf("DeepSeek API error for %s: %v", client.Username, err)
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

			// Save AI response to DB
			if aiContent != "" {
				if err := app.conversations.AddMessage(convID, "assistant", aiContent); err != nil {
					log.Printf("Failed to save AI response: %v", err)
				}
			}

			// Send stream_end
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

// ─── Static File Serving ──────────────────────────────────────

func (app *App) serveStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/login.css" || path == "/login.js" || path == "/health" {
		http.ServeFile(w, r, "./static"+path)
		return
	}

	session := app.getSession(r)
	if session == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	http.ServeFile(w, r, "./static"+path)
}

// ─── Helpers ──────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ─── MySQL Init ───────────────────────────────────────────────

func initMySQL() (*sql.DB, error) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		return nil, fmt.Errorf("MYSQL_DSN environment variable not set")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping mysql: %w", err)
	}

	log.Println("MySQL connected")
	return db, nil
}

// ─── Main ─────────────────────────────────────────────────────

func main() {
	initDeepSeek()

	db, err := initMySQL()
	if err != nil {
		log.Fatalf("MySQL init failed: %v", err)
	}
	defer db.Close()

	app := &App{
		hub:           newHub(),
		users:         newUserStore(),
		sessions:      newSessionStore(),
		conversations: newConversationStore(db),
		db:            db,
	}
	go app.hub.run()

	// Background session cleanup every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			app.sessions.Cleanup()
		}
	}()

	// Public routes
	http.HandleFunc("/login", app.handleLogin)
	http.HandleFunc("/login.css", app.serveStatic)
	http.HandleFunc("/login.js", app.serveStatic)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
	})

	// Auth API
	http.HandleFunc("/api/login", app.handleLogin)
	http.HandleFunc("/api/logout", app.handleLogout)
	http.HandleFunc("/api/session", app.handleSession)

	// Conversation API (protected)
	http.HandleFunc("GET /api/conversations", app.authMiddleware(app.handleListConversations))
	http.HandleFunc("POST /api/conversations", app.authMiddleware(app.handleCreateConversation))
	http.HandleFunc("GET /api/conversations/{id}/messages", app.authMiddleware(app.handleConversationMessages))
	http.HandleFunc("DELETE /api/conversations/{id}", app.authMiddleware(app.handleDeleteConversation))
	http.HandleFunc("PUT /api/conversations/{id}", app.authMiddleware(app.handleRenameConversation))

	// Protected routes
	http.HandleFunc("/ws", app.authMiddleware(app.handleWS))
	http.HandleFunc("/", app.authMiddleware(app.handleChat))
	http.HandleFunc("/style.css", app.serveStatic)
	http.HandleFunc("/app.js", app.serveStatic)

	addr := ":8080"
	log.Printf("Chat server starting on %s", addr)
	log.Printf("Login page: /login")
	log.Printf("Chat page:  /")
	log.Printf("WebSocket:  /ws")
	log.Printf("MySQL:      connected")
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
