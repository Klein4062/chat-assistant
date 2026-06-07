package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ═══════════════════════════════════════════════════════════════
// 用户存储（内存）
// ═══════════════════════════════════════════════════════════════

// newUserStore 创建用户存储并预置初始账号。
func newUserStore() *UserStore {
	us := &UserStore{users: make(map[string]string)}
	// 预生成的 bcrypt 哈希（cost=12）
	us.users["Klein4062"] = "$2b$12$c.8cW/ZBKNpbcpfOYNg3E.5.yMdFf84.LmXd.qJ1WPVvrHFQvpxg6"
	return us
}

// Validate 验证用户名和密码。返回 true 表示凭据正确。
func (us *UserStore) Validate(username, password string) bool {
	us.mu.RLock()
	hash, ok := us.users[username]
	us.mu.RUnlock()
	if !ok {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ═══════════════════════════════════════════════════════════════
// 会话存储（内存）
// ═══════════════════════════════════════════════════════════════

// newSessionStore 创建空的会话存储。
func newSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]*Session)}
}

// Create 为新登录用户创建会话，返回随机 token。
func (ss *SessionStore) Create(username string) string {
	token := generateToken()
	now := time.Now()
	ss.mu.Lock()
	ss.sessions[token] = &Session{Username: username, CreatedAt: now, LastActivity: now}
	ss.mu.Unlock()
	return token
}

// Get 根据 token 获取会话，不存在时返回 nil。
func (ss *SessionStore) Get(token string) *Session {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.sessions[token]
}

// Touch 刷新会话的最后活跃时间。
func (ss *SessionStore) Touch(token string) {
	ss.mu.Lock()
	if s, ok := ss.sessions[token]; ok {
		s.LastActivity = time.Now()
	}
	ss.mu.Unlock()
}

// Delete 删除指定会话。
func (ss *SessionStore) Delete(token string) {
	ss.mu.Lock()
	delete(ss.sessions, token)
	ss.mu.Unlock()
}

// Cleanup 清理所有过期会话（空闲过期或绝对过期）。每 5 分钟由后台 goroutine 调用。
func (ss *SessionStore) Cleanup() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for token, s := range ss.sessions {
		if s.IsIdleExpired() || s.IsExpired() {
			delete(ss.sessions, token)
			log.Printf("会话过期: %s（空闲 %v）", s.Username, time.Since(s.LastActivity).Round(time.Second))
		}
	}
}

// generateToken 生成 32 字节随机 hex 字符串作为会话 token。
func generateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ═══════════════════════════════════════════════════════════════
// 对话存储（MySQL + 内存缓存）
// ═══════════════════════════════════════════════════════════════

// newConversationStore 创建对话存储。
func newConversationStore(db *sql.DB) *ConversationStore {
	return &ConversationStore{
		cache:    make(map[int64][]ChatMessage),
		maxTurns: maxHistoryTurns,
		db:       db,
	}
}

// AddMessage 保存消息到 MySQL 并更新内存缓存。
func (cs *ConversationStore) AddMessage(convID int64, role, content string) error {
	// 写入 MySQL
	_, err := cs.db.Exec(
		"INSERT INTO messages (conversation_id, role, content) VALUES (?, ?, ?)",
		convID, role, content,
	)
	if err != nil {
		return fmt.Errorf("保存消息失败: %w", err)
	}

	// 更新内存缓存
	cs.mu.Lock()
	msgs := cs.cache[convID]
	msgs = append(msgs, ChatMessage{Role: role, Content: content})
	// 超出上限时裁剪最旧的用户-助手对话对
	if len(msgs) > cs.maxTurns*2+1 {
		cut := len(msgs) - cs.maxTurns*2
		if cut < 1 {
			cut = 1
		}
		msgs = append(msgs[:1], msgs[cut:]...)
	}
	cs.cache[convID] = msgs
	cs.mu.Unlock()

	// 更新会话时间戳
	cs.db.Exec("UPDATE conversations SET updated_at = NOW() WHERE id = ?", convID)
	return nil
}

// GetHistory 获取会话的最近消息（先查缓存，未命中则查 MySQL）。
func (cs *ConversationStore) GetHistory(convID int64) ([]ChatMessage, error) {
	// 命中缓存直接返回
	cs.mu.RLock()
	cached, ok := cs.cache[convID]
	cs.mu.RUnlock()
	if ok {
		return cached, nil
	}

	// 从 MySQL 加载最近 N 轮（倒序查询后翻转）
	rows, err := cs.db.Query(
		`SELECT role, content FROM messages
		 WHERE conversation_id = ?
		 ORDER BY id DESC LIMIT ?`,
		convID, cs.maxTurns*2,
	)
	if err != nil {
		return nil, fmt.Errorf("加载消息失败: %w", err)
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
	// 翻转为时间正序
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	// 写入缓存
	cs.mu.Lock()
	cs.cache[convID] = msgs
	cs.mu.Unlock()
	return msgs, nil
}

// ClearCache 清除指定会话的缓存（删除会话时调用）。
func (cs *ConversationStore) ClearCache(convID int64) {
	cs.mu.Lock()
	delete(cs.cache, convID)
	cs.mu.Unlock()
}

// ═══════════════════════════════════════════════════════════════
// 会话数据库操作
// ═══════════════════════════════════════════════════════════════

// listConversations 列出用户的所有会话，按最近更新倒序。
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
		return nil, fmt.Errorf("查询会话列表失败: %w", err)
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
		list = []ConversationInfo{} // 返回空数组而非 null
	}
	return list, nil
}

// createConversation 创建新会话，检查上限（最多 3 个）。
func (app *App) createConversation(username, title string) (*ConversationInfo, error) {
	// 检查数量上限
	var count int
	if err := app.db.QueryRow(
		"SELECT COUNT(*) FROM conversations WHERE username = ?", username,
	).Scan(&count); err != nil {
		return nil, fmt.Errorf("统计会话数失败: %w", err)
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
		return nil, fmt.Errorf("创建会话失败: %w", err)
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

// deleteConversation 删除会话及关联消息（级联），验证所有权。
func (app *App) deleteConversation(id int64, username string) error {
	// 验证所有权
	var owner string
	if err := app.db.QueryRow(
		"SELECT username FROM conversations WHERE id = ?", id,
	).Scan(&owner); err != nil {
		return fmt.Errorf("会话不存在: %w", err)
	}
	if owner != username {
		return fmt.Errorf("无权操作此会话")
	}

	if _, err := app.db.Exec("DELETE FROM conversations WHERE id = ?", id); err != nil {
		return fmt.Errorf("删除会话失败: %w", err)
	}

	app.conversations.ClearCache(id)
	return nil
}

// renameConversation 重命名会话，验证所有权。
func (app *App) renameConversation(id int64, username, title string) error {
	var owner string
	if err := app.db.QueryRow(
		"SELECT username FROM conversations WHERE id = ?", id,
	).Scan(&owner); err != nil {
		return fmt.Errorf("会话不存在: %w", err)
	}
	if owner != username {
		return fmt.Errorf("无权操作此会话")
	}

	if _, err := app.db.Exec(
		"UPDATE conversations SET title = ? WHERE id = ?", title, id,
	); err != nil {
		return fmt.Errorf("重命名失败: %w", err)
	}
	return nil
}

// loadMessages 加载会话消息（先验证所有权）。
func (app *App) loadMessages(convID int64, username string) ([]ChatMessage, error) {
	var owner string
	if err := app.db.QueryRow(
		"SELECT username FROM conversations WHERE id = ?", convID,
	).Scan(&owner); err != nil {
		return nil, fmt.Errorf("会话不存在: %w", err)
	}
	if owner != username {
		return nil, fmt.Errorf("无权访问此会话")
	}

	return app.conversations.GetHistory(convID)
}
