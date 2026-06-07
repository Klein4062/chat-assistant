package main

import (
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
	searchURL := fmt.Sprintf("https://cn.bing.com/search?q=%s&count=10", url.QueryEscape(query))

	req, _ := http.NewRequest("GET", searchURL, nil)
	// 模拟浏览器 User-Agent，避免被反爬
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
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

// buildSearchContext 将搜索结果拼接为一条用户消息，注入对话上下文。
// 这样 AI 能基于搜索内容回答，并自然引用来源。
func buildSearchContext(history []ChatMessage, results []SearchResult) []ChatMessage {
	var sb strings.Builder
	sb.WriteString("以下是从网络搜索到的相关信息，请基于这些信息回答用户问题：\n\n")
	for i, r := range results {
		if i >= 5 {
			break
		}
		sb.WriteString(fmt.Sprintf("【%d】%s\n%s\n\n", i+1, r.Title, r.Snippet))
	}
	enhanced := make([]ChatMessage, 0, len(history)+1)
	enhanced = append(enhanced, ChatMessage{Role: "user", Content: sb.String()})
	enhanced = append(enhanced, history...)
	return enhanced
}
