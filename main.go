package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
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
func callOpenClawStream(username string, conversationID int64, app *App, userMessage string, imageURL string, history []ChatMessage, sendChunk func(string) error) error {
	if !openclawEnabled || openclawAuthToken == "" {
		// 回退到直连 DeepSeek（需要完整 history）
		return callDeepSeekStream(history, nil, sendChunk, nil)
	}

	// 组装请求：system prompt + 仅当前消息
	// OpenClaw 通过 session 自行维护上下文，无需每次发送完整历史
	msgContent := userMessage
	if imageURL != "" {
		// 调用免费图片识别服务获取描述
		desc := describeImage(imageURL)
		if desc != "" {
			msgContent = userMessage + "\n\n[用户上传的图片描述：" + desc + "]\n请基于以上图片描述回复用户。"
		} else {
			msgContent = userMessage + "\n\n（用户上传了一张图片，暂时无法识别图片内容。请告知用户。）"
		}
	}
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: msgContent},
	}
	reqBody := ChatCompletionRequest{
		Model:    "openclaw/default",
		Messages: messages,
		Stream:   true,
		User:     fmt.Sprintf("%s-conv-%d", username, conversationID),
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

// describeImage 获取图片描述。
// 优先级：VISION_API_URL 自定义服务 > HuggingFace > 本地基础分析。
func describeImage(imagePath string) string {
	data, err := os.ReadFile("." + imagePath)
	if err != nil {
		log.Printf("读取图片失败 %s: %v", imagePath, err)
		return ""
	}

	// 1. 自定义图片识别 API（通过 VISION_API_URL 环境变量配置）
	if visionAPIURL := os.Getenv("VISION_API_URL"); visionAPIURL != "" {
		desc := callVisionAPI(visionAPIURL, data)
		if desc != "" {
			return desc
		}
	}

	// 2. HuggingFace 免费推理 API
	desc := callHuggingFaceVision(data)
	if desc != "" {
		return desc
	}

	// 3. 本地基础分析（纯 Go，零依赖）
	desc = analyzeImageLocal(data)
	if desc != "" {
		return desc
	}

	return ""
}

// analyzeImageLocal 纯 Go 基础图片分析：尺寸、格式、主色调。
func analyzeImageLocal(data []byte) string {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}

	// 解码完整图片以分析颜色
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Sprintf("一张 %s 格式图片，尺寸 %dx%d", format, cfg.Width, cfg.Height)
	}

	// 采样分析主色调
	bounds := img.Bounds()
	var totalR, totalG, totalB uint64
	sampleStep := max(1, (bounds.Dx()*bounds.Dy())/400) // 最多采样约 400 像素
	count := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += max(1, bounds.Dy()/20) {
		for x := bounds.Min.X; x < bounds.Max.X; x += max(1, bounds.Dx()/20) {
			r, g, b, _ := img.At(x, y).RGBA()
			totalR += uint64(r >> 8)
			totalG += uint64(g >> 8)
			totalB += uint64(b >> 8)
			count++
		}
	}
	_ = sampleStep

	avgR := int(totalR / uint64(count))
	avgG := int(totalG / uint64(count))
	avgB := int(totalB / uint64(count))

	// 推测主色调名称
	colorName := rgbToColorName(avgR, avgG, avgB)
	brightness := "中等亮度"
	if avgR+avgG+avgB > 600 {
		brightness = "偏亮"
	} else if avgR+avgG+avgB < 200 {
		brightness = "偏暗"
	}

	desc := fmt.Sprintf("一张 %s 格式图片，尺寸 %dx%d，主色调为%s（%s）",
		format, cfg.Width, cfg.Height, colorName, brightness)
	log.Printf("本地图片分析: %s", desc)
	return desc
}

// rgbToColorName 根据 RGB 值推测中文颜色名。
func rgbToColorName(r, g, b int) string {
	// 灰度
	if abs(r-g) < 20 && abs(g-b) < 20 && abs(r-b) < 20 {
		if r < 40 {
			return "黑色"
		} else if r < 100 {
			return "深灰色"
		} else if r < 180 {
			return "灰色"
		} else if r < 230 {
			return "浅灰色"
		}
		return "白色"
	}
	// 彩色
	switch {
	case r > g && r > b && r-b > 40:
		if g > 100 {
			return "橙红色"
		}
		return "红色"
	case g > r && g > b && g-r > 30:
		return "绿色"
	case b > r && b > g && b-r > 30:
		return "蓝色"
	case r > 150 && g > 150 && b < 100:
		return "黄色"
	case r > 150 && g < 100 && b > 150:
		return "紫色"
	case r < 100 && g > 100 && b > 100:
		return "青色"
	default:
		return "混合色调"
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// callVisionAPI 调用自定义图片识别 API（兼容 OpenAI vision / 自定义格式）。
func callVisionAPI(apiURL string, imageData []byte) string {
	b64 := base64.StdEncoding.EncodeToString(imageData)
	body, _ := json.Marshal(map[string]interface{}{
		"image":  b64,
		"format": "png",
	})
	resp, err := http.Post(apiURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("自定义 Vision API 不可用: %v", err)
		return ""
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		log.Printf("自定义 Vision API 返回 %d", resp.StatusCode)
		return ""
	}
	// 尝试解析常见返回格式
	var result struct {
		Description string `json:"description"`
		Caption     string `json:"caption"`
		Text        string `json:"text"`
	}
	if json.Unmarshal(respBody, &result) == nil {
		desc := result.Description
		if desc == "" {
			desc = result.Caption
		}
		if desc == "" {
			desc = result.Text
		}
		if desc != "" {
			log.Printf("自定义 Vision API 成功: %s", truncate(desc, 80))
			return desc
		}
	}
	// 纯文本返回
	desc := strings.TrimSpace(string(respBody))
	if len(desc) > 0 && len(desc) < 500 {
		log.Printf("Vision API 纯文本: %s", truncate(desc, 80))
		return desc
	}
	return ""
}

// callHuggingFaceVision 使用 Hugging Face 免费推理 API 识别图片内容。
func callHuggingFaceVision(imageData []byte) string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(
		"https://api-inference.huggingface.co/models/Salesforce/blip-image-captioning-base",
		"image/png",
		bytes.NewReader(imageData),
	)
	if err != nil {
		log.Printf("HuggingFace 不可用: %v", err)
		return ""
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		msg := string(respBody)
		if strings.Contains(msg, "loading") || strings.Contains(msg, "currently loading") {
			log.Printf("HuggingFace 模型冷启动加载中，下次请求可用")
		} else {
			log.Printf("HuggingFace 返回 %d: %s", resp.StatusCode, truncate(msg, 100))
		}
		return ""
	}

	var result []struct {
		GeneratedText string `json:"generated_text"`
	}
	if json.Unmarshal(respBody, &result) == nil && len(result) > 0 {
		log.Printf("图片识别成功(HF): %s", truncate(result[0].GeneratedText, 80))
		return result[0].GeneratedText
	}
	return ""
}

// buildMultimodalRequest 构造包含图片的多模态请求 JSON（备用，DeepSeek 支持 vision 后启用）。
// 图片以 base64 data URL 内联，避免外部 URL 无法访问的问题。
func buildMultimodalRequest(systemPrompt, userMessage, imagePath, model, user string) []byte {
	// 读取图片并转 base64
	imageData, err := os.ReadFile("." + imagePath)
	if err != nil {
		// 回退：仅发送文本
		log.Printf("读取图片失败 %s: %v，回退为纯文本", imagePath, err)
		b, _ := json.Marshal(ChatCompletionRequest{
			Model: model,
			Messages: []ChatMessage{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: userMessage + "\n（图片未能加载，请直接回复文本）"},
			},
			Stream: true,
			User:   user,
		})
		return b
	}

	// 检测 MIME 类型
	mimeType := "image/png"
	switch {
	case strings.HasSuffix(imagePath, ".jpg") || strings.HasSuffix(imagePath, ".jpeg"):
		mimeType = "image/jpeg"
	case strings.HasSuffix(imagePath, ".gif"):
		mimeType = "image/gif"
	case strings.HasSuffix(imagePath, ".webp"):
		mimeType = "image/webp"
	}

	base64Data := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(imageData)

	body := map[string]interface{}{
		"model":  model,
		"stream": true,
		"user":   user,
		"messages": []map[string]interface{}{
			{
				"role":    "system",
				"content": systemPrompt,
			},
			{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": userMessage,
					},
					{
						"type": "image_url",
						"image_url": map[string]string{
							"url": base64Data,
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
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
	// 上传的图片公开访问
	http.HandleFunc("/uploads/", app.serveUpload)
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

	// 图片上传（需登录）
	http.HandleFunc("POST /api/upload", app.authMiddleware(app.handleUpload))

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
