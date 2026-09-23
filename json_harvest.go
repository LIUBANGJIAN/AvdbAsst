package main

// 上游 JSON 的防御式解析。
//
// 背景：上游没有公开"目录清单"的响应结构承诺，同一个语义在不同版本里可能是
// ["/media/tv"]、[{"path":"/media/tv","name":"剧集"}]，甚至嵌在
// {"data":{"items":[...]}} 里。硬编码一种结构必然在某个版本上失效。
//
// 安全红线（比"解析得全"更重要）：
//   上游响应里可能含 115 的 Cookie、网盘账号、Emby 密钥等敏感值。
//   因此这里**只按白名单字段名取值**，绝不把整个对象当成字符串集合收上来，
//   也绝不把原始响应回传给浏览器。宁可少解析，也不能把凭据泄露到前端。

import (
	"encoding/json"
	"strings"
)

// maxHarvested 是单次解析收集条目的上限，避免异常上游把页面撑爆。
const maxHarvested = 50

// directoryWrapperKeys 是目录清单可能被包在哪些字段名下。
// 只认这些名字，不做"取遍所有字符串"的兜底——那会把 Cookie 之类的凭据一起收上来。
var directoryWrapperKeys = []string{
	"data", "directories", "dirs", "folders", "paths", "items", "list", "result", "results",
}

// directoryPathKeys 是"一个目录条目"里承载**真实路径**的字段名。
var directoryPathKeys = []string{"path", "directory", "dir", "folder", "full_path", "value"}

// directoryNameKeys 是仅在条目**没有**路径字段时才启用的兜底字段。
//
// 为什么要分开：条目常见形如 {"path": "/media/tv", "name": "剧集"}，
// 若两者都收，"剧集"就会变成一条假的保存路径。只在完全没有 path 类字段时
// 才退回 name，才既保住"有些上游只给显示名"的兼容性，又不制造噪音。
var directoryNameKeys = []string{"name", "label", "title"}

// harvestStrings 从目录类响应里收集字符串清单。
func harvestStrings(body []byte) []string {
	var node any
	if err := json.Unmarshal(body, &node); err != nil {
		return nil
	}

	var out []string
	seen := make(map[string]struct{})
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || len(out) >= maxHarvested {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	if s, ok := node.(string); ok { // 极端情况：整个响应就是一个裸字符串
		add(s)
		return out
	}

	collectStrings(node, 0, add)
	return out
}

// collectStrings 在**白名单字段**范围内递归收集字符串。
func collectStrings(node any, depth int, add func(string)) {
	if depth > 4 {
		return
	}
	switch v := node.(type) {
	case string:
		add(v)
	case []any:
		for _, item := range v {
			collectStrings(item, depth+1, add)
		}
	case map[string]any:
		hasPath := false
		for _, key := range directoryPathKeys {
			if child, ok := v[key].(string); ok {
				add(child)
				hasPath = true
			}
		}
		if !hasPath {
			for _, key := range directoryNameKeys {
				if child, ok := v[key].(string); ok {
					add(child)
				}
			}
		}
		for _, key := range directoryWrapperKeys {
			if child, ok := v[key]; ok {
				collectStrings(child, depth+1, add)
			}
		}
	}
}

// truncateForDisplay 截断可能进入界面的上游文本，避免糊满页面。
func truncateForDisplay(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 {
		limit = 200
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
