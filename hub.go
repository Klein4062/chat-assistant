package main

import (
	"log"
)

// newHub 创建 WebSocket 连接管理中心。
func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// run 是 Hub 的主循环，在独立 goroutine 中运行。
// 通过 channel 处理客户端的注册、注销和广播，避免并发问题。
func (h *Hub) run() {
	for {
		select {
		// ── 客户端连接 ──────────────────────────────────────
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			count := len(h.clients)
			h.mu.Unlock()
			log.Printf("[%s] 已连接（在线 %d 人）", client.Username, count)

		// ── 客户端断开 ──────────────────────────────────────
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.Send) // 关闭发送通道以终止 write pump
			}
			count := len(h.clients)
			h.mu.Unlock()
			log.Printf("[%s] 已断开（在线 %d 人）", client.Username, count)

		// ── 广播消息 ────────────────────────────────────────
		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.Send <- message:
				default:
					// 发送通道已满，视为客户端已掉线
					close(client.Send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
		}
	}
}
