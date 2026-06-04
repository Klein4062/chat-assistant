package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

// ─── Data Types ────────────────────────────────────────────────

type Message struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	Sender    string `json:"sender,omitempty"`
	Username  string `json:"username,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
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
	sessionMaxAge   = 24 * time.Hour  // absolute max session lifetime
	sessionIdleTimeout = 10 * time.Minute // idle timeout
)

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

// ─── Conversation Store ─────────────────────────────────────────

type ConversationStore struct {
	mu       sync.RWMutex
	history  map[string][]ChatMessage // clientID → messages
	maxTurns int                      // keep last N user+assistant turns
}

func newConversationStore() *ConversationStore {
	return &ConversationStore{
		history:  make(map[string][]ChatMessage),
		maxTurns: 20,
	}
}

func (cs *ConversationStore) Add(clientID, role, content string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	msgs := cs.history[clientID]
	msgs = append(msgs, ChatMessage{Role: role, Content: content})
	if len(msgs) > cs.maxTurns*2+1 { // system + N turns * 2
		// Trim oldest user+assistant pair, keep system prompt
		cut := len(msgs) - cs.maxTurns*2
		if cut < 1 {
			cut = 1
		}
		msgs = append(msgs[:1], msgs[cut:]...)
	}
	cs.history[clientID] = msgs
}

func (cs *ConversationStore) Get(clientID string) []ChatMessage {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.history[clientID]
}

func (cs *ConversationStore) Clear(clientID string) {
	cs.mu.Lock()
	delete(cs.history, clientID)
	cs.mu.Unlock()
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

	// Parse SSE stream
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
			continue // skip malformed chunks
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
	users map[string]string // username → bcrypt hash
}

func newUserStore() *UserStore {
	us := &UserStore{users: make(map[string]string)}
	// Preload from MySQL or hardcoded initial user
	// Pre-generated bcrypt hash for initial user
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
	sessions map[string]*Session // token → session
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

// Cleanup removes expired sessions (both idle and max-age). Runs periodically.
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
	hub          *Hub
	users        *UserStore
	sessions     *SessionStore
	conversations *ConversationStore
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
			// For API calls, return 401. For page requests, redirect to /login.
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws") {
				http.Error(w, `{"error":"unauthorized","reason":"session_expired"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?expired=1", http.StatusFound)
			return
		}
		// Refresh last activity time
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

// ─── Handlers ──────────────────────────────────────────────────

func (app *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// Serve login page
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
		MaxAge:   86400, // 24 hours
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

func (app *App) handleChat(w http.ResponseWriter, r *http.Request) {
	// Auth already checked by authMiddleware
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
			app.conversations.Clear(client.ID)
			conn.Close()
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

			userContent := msg.Content

			// Save user message to conversation history
			app.conversations.Add(client.ID, "user", userContent)

			// Send stream_start to indicate AI is thinking
			startMsg, _ := json.Marshal(Message{
				Type:     "stream_start",
				Sender:   "server",
				Username: "AI",
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			client.Send <- startMsg

			// Call DeepSeek API with streaming
			history := app.conversations.Get(client.ID)
			var aiContent string

			err = callDeepSeekStream(history, func(chunk string) error {
				aiContent += chunk
				chunkMsg, _ := json.Marshal(Message{
					Type:      "stream_chunk",
					Content:   chunk,
					Sender:    "server",
					Username:  "AI",
					Timestamp: time.Now().UTC().Format(time.RFC3339),
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
					Type:      "stream_chunk",
					Content:   fmt.Sprintf("抱歉，AI 服务暂时不可用：%v", err),
					Sender:    "server",
					Username:  "AI",
					Timestamp: time.Now().UTC().Format(time.RFC3339),
				})
				client.Send <- errMsg
				aiContent = fmt.Sprintf("[错误] %v", err)
			}

			// Save AI response to history
			if aiContent != "" {
				app.conversations.Add(client.ID, "assistant", aiContent)
			}

			// Send stream_end
			endMsg, _ := json.Marshal(Message{
				Type:      "stream_end",
				Content:   aiContent,
				Sender:    "server",
				Username:  "AI",
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			client.Send <- endMsg
		}
	}()
}

// ─── Static File Serving ──────────────────────────────────────

func (app *App) serveStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Public files (login page assets, health)
	if path == "/login.css" || path == "/login.js" || path == "/health" {
		http.ServeFile(w, r, "./static"+path)
		return
	}

	// Protected — require auth
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

// ─── Main ─────────────────────────────────────────────────────

func main() {
	initDeepSeek()

	app := &App{
		hub:           newHub(),
		users:         newUserStore(),
		sessions:      newSessionStore(),
		conversations: newConversationStore(),
	}
	go app.hub.run()

	// Background session cleanup every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			app.sessions.Cleanup()
		}
	}()

	// Public routes (no auth required)
	http.HandleFunc("/login", app.handleLogin)
	http.HandleFunc("/login.css", app.serveStatic)
	http.HandleFunc("/login.js", app.serveStatic)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)})
	})

	// Auth API (no middleware — they handle auth internally)
	http.HandleFunc("/api/login", app.handleLogin)
	http.HandleFunc("/api/logout", app.handleLogout)
	http.HandleFunc("/api/session", app.handleSession)

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
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
