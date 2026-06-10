package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// bingClient 模拟浏览器行为：强制 HTTP/1.1，避免 TLS 指纹被识别为爬虫。
var bingClient = &http.Client{
	Timeout: 12 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ForceAttemptHTTP2: false, // 强制 HTTP/1.1
	},
}

// ═══════════════════════════════════════════════════════════════
// 联网搜索 — 多后端 fallback
// ═══════════════════════════════════════════════════════════════

// searchWeb 执行网络搜索，按优先级尝试多个后端：
//  1. Serper.dev（Google 搜索，需 SERPER_API_KEY，免费 100次/月）
//  2. SEARCH_API_URL（自定义 SearXNG/DuckDuckGo 兼容端点）
//  3. Bing 内置（零配置、免费、国内可用）
func searchWeb(query string) []SearchResult {
	// 第一优先级：Serper.dev
	if key := os.Getenv("SERPER_API_KEY"); key != "" {
		if r := searchSerper(query, key); len(r) > 0 {
			return r
		}
	}
	// 第二优先级：自定义搜索 API
	if u := os.Getenv("SEARCH_API_URL"); u != "" {
		if r := searchCustomAPI(query, u); len(r) > 0 {
			return r
		}
	}
	// 默认：Bing 内置搜索
	return searchBing(query)
}

// ═══════════════════════════════════════════════════════════════
// Serper.dev Google 搜索
// ═══════════════════════════════════════════════════════════════

func searchSerper(query, apiKey string) []SearchResult {
	reqBody, _ := json.Marshal(map[string]string{"q": query, "gl": "cn", "hl": "zh-cn"})
	req, _ := http.NewRequest("POST", "https://google.serper.dev/search", strings.NewReader(string(reqBody)))
	req.Header.Set("X-API-KEY", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024)) // 限制 256KB

	var r struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if json.Unmarshal(body, &r) == nil {
		var results []SearchResult
		for _, o := range r.Organic {
			results = append(results, SearchResult{Title: o.Title, URL: o.Link, Snippet: o.Snippet})
		}
		return results
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════
// 自定义搜索 API（SearXNG / DuckDuckGo 兼容格式）
// ═══════════════════════════════════════════════════════════════

func searchCustomAPI(query, apiURL string) []SearchResult {
	fullURL := fmt.Sprintf(apiURL, url.QueryEscape(query))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(fullURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))

	var sr struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if json.Unmarshal(body, &sr) == nil {
		var results []SearchResult
		for _, r := range sr.Results {
			results = append(results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
		}
		return results
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════
// Bing 搜索（内置，零配置，国内可用）
// ═══════════════════════════════════════════════════════════════

// searchBing 从 cn.bing.com 抓取搜索结果 HTML 并解析。
func searchBing(query string) []SearchResult {
	// 中文查询：去口语虚词 + site: 限定中文内容源
	q := query
	if containsCJK(query) {
		// 去掉干扰词：几号、如何、怎么、吗、呢、吧、啊、请、帮我等
		q = cleanQuery(query)
		q = "site:bilibili.com " + q
	}
	searchURL := fmt.Sprintf("https://cn.bing.com/search?q=%s&count=10&setlang=zh-cn", url.QueryEscape(q))

	req, _ := http.NewRequest("GET", searchURL, nil)
	// 完整模拟浏览器请求头，防止 Bing 通过 TLS 指纹识别为爬虫
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("DNT", "1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	resp, err := bingClient.Do(req)
	if err != nil {
		log.Printf("Bing 搜索失败: %v", err)
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024)) // 限制 512KB
	if err != nil {
		return nil
	}

	results := parseBingResults(string(body))
	// DEBUG: save HTML for analysis
	if len(results) == 0 && len(body) > 100 {
		os.WriteFile("/tmp/bing-debug.html", body, 0644)
		log.Printf("Bing HTML saved to /tmp/bing-debug.html (%d bytes)", len(body))
	}
	if len(results) > 0 {
		log.Printf("Bing 搜索: %d 条结果 ← %q", len(results), truncate(query, 50))
	}
	return results
}

// parseBingResults 从 Bing HTML 中提取搜索结果。
// Bing 的每条结果在 <li class="b_algo"> 块中，包含标题链接和摘要。
func parseBingResults(html string) []SearchResult {
	var results []SearchResult
	seen := make(map[string]bool) // 去重

	// 正则：提取每个 <li class="b_algo"> 块
	blockRe := regexp.MustCompile(`<li class="b_algo"[^>]*>(.*?)</li>`)
	// 正则：从块中提取标题链接
	linkRe := regexp.MustCompile(`<h2[^>]*><a[^>]*href="(https?://[^"]+)"[^>]*>(.*?)</a></h2>`)
	// 正则：提取摘要（两种可能的 class 名）
	snippetRe := regexp.MustCompile(`<p[^>]*class="[^"]*b_lineclamp[^"]*"[^>]*>(.*?)</p>`)
	altSnippetRe := regexp.MustCompile(`<p[^>]*class="[^"]*b_algoSlug[^"]*"[^>]*>(.*?)</p>`)

	blocks := blockRe.FindAllStringSubmatch(html, 20)
	for _, block := range blocks {
		if len(block) < 2 {
			continue
		}
		content := block[1]

		// 提取标题和 URL
		m := linkRe.FindStringSubmatch(content)
		if m == nil || len(m) < 3 {
			continue
		}
		link := strings.TrimSpace(m[1])
		title := strings.TrimSpace(stripTags(m[2]))

		// 跳过内部链接和重复结果
		if strings.Contains(link, "bing.com") || strings.Contains(link, "microsoft.com") {
			continue
		}
		if seen[link] || title == "" {
			continue
		}
		seen[link] = true

		// 提取摘要
		snippet := ""
		if sm := snippetRe.FindStringSubmatch(content); sm != nil && len(sm) > 1 {
			snippet = strings.TrimSpace(stripTags(sm[1]))
		} else if sm := altSnippetRe.FindStringSubmatch(content); sm != nil && len(sm) > 1 {
			snippet = strings.TrimSpace(stripTags(sm[1]))
		}

		results = append(results, SearchResult{
			Title:   title,
			URL:     link,
			Snippet: snippet,
		})
		if len(results) >= 5 {
			break
		}
	}
	return results
}

// ═══════════════════════════════════════════════════════════════
// 工具函数
// ═══════════════════════════════════════════════════════════════

// cleanQuery 去除中文口语虚词，提取搜索关键词。
// "今天几号，北京天气如何" → "今天 北京天气"
func cleanQuery(q string) string {
	// 口语虚词/干扰词——长词优先，避免"怎么"误删"怎么样"的残留
	noise := []string{"怎么样", "能不能", "可不可以", "怎么", "几号", "如何",
		"请", "帮我", "可以", "能否", "麻烦", "一下", "谢谢",
		"吗", "呢", "吧", "啊", "呀", "哦"}
	for _, w := range noise {
		q = strings.ReplaceAll(q, w, "")
	}
	// 去掉多余空格和标点
	q = strings.TrimSpace(q)
	q = regexp.MustCompile(`[，。！？、；：""''【】《》（）\s]+`).ReplaceAllString(q, " ")
	return strings.TrimSpace(q)
}

// containsCJK 判断字符串是否包含中日韩统一表意文字。
func containsCJK(s string) bool {
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) {
			return true
		}
	}
	return false
}

// stripTags 移除 HTML 标签，保留纯文本。
func stripTags(s string) string {
	return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(s, "")
}

// truncate 截断字符串到指定长度（按 rune 计，支持中文）。
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// toSearchResultsJSON 将搜索结果序列化为前端可解析的 JSON。
func toSearchResultsJSON(results []SearchResult) string {
	type sr struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	}
	var list []sr
	for _, r := range results {
		list = append(list, sr{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	b, _ := json.Marshal(list)
	return string(b)
}

// buildSearchPrompt 将搜索结果拼接到 system prompt 尾部。
func buildSearchPrompt(basePrompt string, results []SearchResult) string {
	var sb strings.Builder
	sb.WriteString(basePrompt)
	sb.WriteString("\n\n---\n你刚才联网搜索获得了以下结果。直接基于它们回答用户问题，引用时标 [1][2] 等。不要复述或列出搜索结果本身。\n")
	for i, r := range results {
		if i >= 5 {
			break
		}
		sb.WriteString(fmt.Sprintf("[%d] %s | %s\n", i+1, r.Title, r.Snippet))
	}
	return sb.String()
}

// ═══════════════════════════════════════════════════════════════
// 智谱 GLM-4 Web Search（联网搜索 Pro）
// ═══════════════════════════════════════════════════════════════

// searchWebZhipu 使用智谱 GLM-4 的 web_search 工具进行联网搜索。
func searchWebZhipu(query string) ([]SearchResult, string) {
	apiKey := os.Getenv("SEARCH_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("VISION_API_KEY") // 复用视觉 Key
	}
	if apiKey == "" {
		log.Println("SEARCH_API_KEY 未设置，Zhipu 搜索不可用")
		return nil, ""
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"model": "glm-4",
		"messages": []map[string]interface{}{
			{"role": "user", "content": query},
		},
		"tools": []map[string]interface{}{
			{
				"type": "web_search",
				"web_search": map[string]interface{}{
					"enable": true,
				},
			},
		},
		"stream": false,
	})

	httpReq, _ := http.NewRequest("POST", "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		strings.NewReader(string(reqBody)))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("Zhipu 搜索 API 不可用: %v", err)
		return nil, ""
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32768))
	if resp.StatusCode != http.StatusOK {
		log.Printf("Zhipu 搜索返回 %d: %s", resp.StatusCode, truncate(string(respBody), 200))
		return nil, ""
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		WebSearch []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
			Content string `json:"content"`
		} `json:"web_search"`
	}

	if json.Unmarshal(respBody, &result) != nil {
		return nil, ""
	}

	var searchResults []SearchResult
	for _, ws := range result.WebSearch {
		snippet := ws.Snippet
		if snippet == "" {
			snippet = ws.Content
		}
		searchResults = append(searchResults, SearchResult{
			Title:   ws.Title,
			URL:     ws.Link,
			Snippet: snippet,
		})
	}

	summary := ""
	if len(result.Choices) > 0 {
		summary = result.Choices[0].Message.Content
	}

	log.Printf("Zhipu 搜索: %d 条结果 ← %q", len(searchResults), truncate(query, 40))
	return searchResults, summary
}

