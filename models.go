package main

import (
	"database/sql"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ─── 常量 ──────────────────────────────────────────────────────

const (
	sessionMaxAge      = 24 * time.Hour  // 会话绝对有效期
	sessionIdleTimeout = 10 * time.Minute // 空闲超时（10分钟无操作重新登录）
	maxConversations   = 3               // 每用户最多会话数
	maxHistoryTurns    = 20              // AI 对话上下文保留轮数
)

// ─── WebSocket 消息 ────────────────────────────────────────────

// Message 是客户端和服务端之间的 WebSocket 消息格式。
type Message struct {
	Type           string `json:"type"`                       // 消息类型：message / stream_start / stream_chunk / stream_end / search_results / error
	Content        string `json:"content,omitempty"`          // 消息正文
	Sender         string `json:"sender,omitempty"`           // 发送方标识（server / user）
	Username       string `json:"username,omitempty"`         // 用户名/AI名
	Timestamp      string `json:"timestamp,omitempty"`        // ISO8601 时间戳
	ConversationID int64  `json:"conversation_id,omitempty"`  // 所属会话 ID
	EnableSearch   bool   `json:"enable_search,omitempty"`    // 是否启用联网搜索
}

// ─── 登录认证 ──────────────────────────────────────────────────

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
	RemainingSecs int    `json:"remaining_secs,omitempty"` // 空闲剩余秒数
}

// ─── 会话管理 ──────────────────────────────────────────────────

// ConversationInfo 是会话列表 API 的返回项（不含消息内容）。
type ConversationInfo struct {
	ID           int64     `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
}

// ─── DeepSeek API ──────────────────────────────────────────────

// ChatMessage 是一条对话记录。
type ChatMessage struct {
	Role    string `json:"role"`    // system / user / assistant
	Content string `json:"content"` // 消息文本
}

// ChatCompletionRequest 是发送给 DeepSeek API 的请求体。
type ChatCompletionRequest struct {
	Model        string        `json:"model"`
	Messages     []ChatMessage `json:"messages"`
	Stream       bool          `json:"stream"`
	EnableSearch bool          `json:"enable_search,omitempty"` // DeepSeek 原生搜索（备用）
}

// ChatCompletionChunk 是 DeepSeek 流式响应的单个 SSE 数据块。
type ChatCompletionChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	SearchResults []SearchResult `json:"search_results,omitempty"` // DeepSeek 原生搜索结果
}

// SearchResult 是一条网络搜索结果。
type SearchResult struct {
	Title   string `json:"title"`   // 结果标题
	URL     string `json:"url"`     // 结果链接
	Snippet string `json:"snippet"` // 结果摘要
}

// ─── WebSocket Hub ─────────────────────────────────────────────

// Client 表示一个 WebSocket 连接。
type Client struct {
	ID       string          // 客户端标识（RemoteAddr）
	Username string          // 登录用户名
	Conn     *websocket.Conn // WebSocket 连接
	Send     chan []byte     // 待发送消息队列
}

// Hub 管理所有活跃的 WebSocket 连接。
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte   // 广播消息通道
	register   chan *Client  // 注册通道
	unregister chan *Client  // 注销通道
	mu         sync.RWMutex  // 保护 clients map
}

// upgrader 将 HTTP 升级为 WebSocket。
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// ─── 会话存储 ──────────────────────────────────────────────────

// Session 表示一个用户登录会话。
type Session struct {
	Username     string    // 用户名
	CreatedAt    time.Time // 创建时间（用于绝对过期判断）
	LastActivity time.Time // 最后活跃时间（用于空闲过期判断）
}

// IsIdleExpired 检查空闲是否超过 10 分钟。
func (s *Session) IsIdleExpired() bool {
	return time.Since(s.LastActivity) > sessionIdleTimeout
}

// IsExpired 检查是否超过 24 小时绝对有效期。
func (s *Session) IsExpired() bool {
	return time.Since(s.CreatedAt) > sessionMaxAge
}

// SessionStore 是内存中的会话存储。
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session // token → session
}

// UserStore 是内存中的用户凭证存储。
type UserStore struct {
	mu    sync.RWMutex
	users map[string]string // username → bcrypt hash
}

// ConversationStore 是会话消息的 MySQL + 内存缓存存储。
type ConversationStore struct {
	mu       sync.RWMutex
	cache    map[int64][]ChatMessage // conversationID → 最近消息缓存
	maxTurns int                     // 缓存轮数上限
	db       *sql.DB                 // MySQL 连接
}

// ─── 应用全局状态 ──────────────────────────────────────────────

// App 持有所有共享状态。
type App struct {
	hub           *Hub
	users         *UserStore
	sessions      *SessionStore
	conversations *ConversationStore
	db            *sql.DB
}

// ─── DeepSeek 配置 ─────────────────────────────────────────────

var (
	deepseekAPIKey  string // API 密钥
	deepseekBaseURL string // API 基础地址
	deepseekModel   string // 模型名称
	systemPrompt    string // 系统提示词
)
