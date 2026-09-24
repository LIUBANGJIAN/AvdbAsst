package main

import (
	"embed"
	"html/template"
	"io/fs"
	"strings"
)

// embeddedWeb 把前端资源直接编进二进制。
// 好处：单文件分发、镜像里不需要额外的文件系统依赖，也不会出现
// "本地能跑、容器里 404" 的经典问题。
//
//go:embed all:web
var embeddedWeb embed.FS

// webRoot 返回以 web/ 为根的只读文件系统。
func webRoot() fs.FS {
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		// 嵌入失败属于编译期资源缺失，没有任何降级方案，直接快速失败。
		panic("嵌入的 web 资源损坏: " + err.Error())
	}
	return sub
}

// staticRoot 返回 web/static，用于挂载 /static/ 前缀。
func staticRoot() fs.FS {
	sub, err := fs.Sub(webRoot(), "static")
	if err != nil {
		panic("嵌入的静态资源目录损坏: " + err.Error())
	}
	return sub
}

// templateFuncs 是模板可用的辅助函数集合。
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatSize": formatSize,
		"formatDate": formatDate,
		// siteName 让页头、标题、描述用同一个产品名，改一处即可全站生效。
		"siteName": func() string { return siteName },
		// pageTitle 生成浏览器标签页的标题。
		//
		// 关键词是否出现由设置页的开关决定，**默认不出现**（见
		// Config.TitleWithKeyword）。两个条件缺一不可：
		//   - withKeyword 为假 → 恒为站名；
		//   - 关键词为空（首页、或只有筛选参数）→ 也只有站名，
		//     否则会渲染出「 · 资源搜索」这种以一个分隔符开头的标题。
		//
		// 返回的字符串仍会被 html/template 按上下文转义，
		// 所以关键词里带尖括号也进不了页面结构。
		"pageTitle": func(keyword string, withKeyword bool) string {
			if k := strings.TrimSpace(keyword); withKeyword && k != "" {
				return k + " · " + siteName
			}
			return siteName
		},
		// add 给结果行编号用：{{add .StartedAt $i}} 算出该行的全局序号，
		// 模板本身没有算术能力。
		"add": func(a, b int) int { return a + b },
		// flags 把布尔标记压成一个以空格分隔的字符串塞进 data-flags。
		//
		// 与服务端筛选共用 torrentFlags：两处各写一份判定的话，
		// "标签显示有、筛选却筛不出来"这种错迟早会出现。
		//
		// 这里**不包含 free**：上游返回的资源全部免费，给每条都挂一个
		// "免费"标记既没有区分度，也没有筛选价值。
		"flags": func(t Torrent) string {
			return strings.Join(torrentFlags(t), " ")
		},
		// dash 让空值在表格里显示为 "-" 而不是空白。
		"dash": func(s string) string {
			if strings.TrimSpace(s) == "" {
				return "-"
			}
			return s
		},
	}
}

// parseTemplates 解析全部页面模板。模板语法错误应当在启动时立刻暴露，
// 所以直接 panic，而不是留到运行时给用户一个 500。
func parseTemplates() *template.Template {
	tpl := template.New("app").Funcs(templateFuncs())
	parsed, err := tpl.ParseFS(webRoot(), "index.html", "settings.html")
	if err != nil {
		panic("解析页面模板失败: " + err.Error())
	}
	return parsed
}
