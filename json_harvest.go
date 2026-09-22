package main

// 上游 JSON 的防御式解析。
//
// 背景：上游没有公开"下载器清单"和"目录清单"的响应结构承诺，同一个语义
// 在不同版本里可能是 "115"、{"id":"115"}、{"downloader_id":"115","name":"115网盘"}
// 甚至嵌在 {"data":{"items":[...]}} 里。硬编码一种结构必然在某个版本上失效。
//
// 安全红线（比"解析得全"更重要）：
//   上游配置里可能含 115 的 Cookie、网盘账号、Emby 密钥等敏感值。
//   因此这里**只按白名单字段名取值**，绝不把整个配置对象当成字符串集合收上来，
//   也绝不把原始配置回传给浏览器。宁可少解析，也不能把凭据泄露到前端。

import (
	"encoding/json"
	"strings"
)

// maxHarvested 是单次解析收集条目的上限，避免异常上游把页面撑爆。
const maxHarvested = 50

// downloaderIDKeys / downloaderLabelKeys 是"下载器标识"与"显示名"的候选字段。
var (
	downloaderIDKeys    = []string{"id", "downloader_id", "downloader", "key", "type", "driver", "name"}
	downloaderLabelKeys = []string{"name", "label", "title", "remark", "type"}
)

// harvestDownloaderOptions 从一段任意 JSON 里收集下载器候选项。
//
// 只把**数组里的元素**当作条目：配置里的清单天然是数组，而散落在普通字段里的
// 单个 "id"/"name" 多半属于无关设置，收上来只会制造噪音。
func harvestDownloaderOptions(raw json.RawMessage) []DownloaderOption {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil
	}

	var out []DownloaderOption
	seen := make(map[string]struct{})

	add := func(id, label string) {
		id = strings.TrimSpace(id)
		if id == "" || len(out) >= maxHarvested {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		label = strings.TrimSpace(label)
		if label == id {
			label = "" // 显示名与标识相同就省掉，避免页面上出现"115（115）"
		}
		out = append(out, DownloaderOption{ID: id, Label: label})
	}

	var walk func(node any, depth int)
	walk = func(node any, depth int) {
		if depth > 4 || len(out) >= maxHarvested {
			return
		}
		switch v := node.(type) {
		case []any:
			for _, item := range v {
				switch elem := item.(type) {
				case string:
					add(elem, "")
				case map[string]any:
					if id, label := downloaderFields(elem); id != "" {
						add(id, label)
					} else {
						walk(elem, depth+1)
					}
				default:
					walk(elem, depth+1)
				}
			}
		case map[string]any:
			for _, child := range v {
				walk(child, depth+1)
			}
		}
	}
	walk(node, 0)
	return out
}

// downloaderFields 从一个对象里按白名单取出下载器标识与显示名。
// 取不到标识就返回空串，调用方据此跳过该对象。
func downloaderFields(m map[string]any) (id, label string) {
	for _, key := range downloaderIDKeys {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			id = strings.TrimSpace(s)
			break
		}
	}
	for _, key := range downloaderLabelKeys {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			label = strings.TrimSpace(s)
			break
		}
	}
	return id, label
}

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
