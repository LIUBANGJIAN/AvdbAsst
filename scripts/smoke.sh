#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# 端到端冒烟测试：对**真实运行中**的 AvdbAsst 发请求，验证核心链路。
#
# 用法：
#   BASE_URL=http://127.0.0.1:8080 ./scripts/smoke.sh
#   BASE_URL=http://127.0.0.1:8080 TOKEN=你的口令 ./scripts/smoke.sh
#   BASE_URL=http://127.0.0.1:8080 QUERY=ABC-123 ./scripts/smoke.sh
#
# 说明：
#   - 全部为只读探测（搜索、配置页、静态资源）。第 10 节虽然会打 /download，
#     但用的都是"必然被拒"的请求（错误方法 / 缺参数 / 跨站），
#     不会触达上游、不会真的提交下载，因此可以放心对生产实例执行。
#   - 鉴权默认走 X-Auth-Token 请求头，避免在 URL 里拼口令；
#     专门的第 9 节会另外验证 ?token= 、Cookie 与免鉴权路径。
#   - 实例若尚未配置上游地址与 API Key，搜索类断言会被自动跳过。
# ---------------------------------------------------------------------------
set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
TOKEN="${TOKEN:-}"
QUERY="${QUERY:-BOBB-456}"

pass=0
fail=0
skip=0

ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=$((fail + 1)); }
note() { printf '  \033[33mSKIP\033[0m  %s\n' "$1"; skip=$((skip + 1)); }

check() {
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1（期望 $3，实际 $2）"; fi
}

# 断言助手统一用 here-string 喂 grep，**不要**用管道。
#
# 原因：脚本开了 `set -o pipefail`，而 `grep -q` 一命中就退出。
# 于是 `printf '%s' "$body" | grep -qF -- "$want"` 里，
# 写端 printf 还在往管道里灌、读端已经走了，printf 收到 SIGPIPE 而死在 141；
# pipefail 把整条管道的状态取成 141，`if` 判为假——
# **命中了却报"未找到"**。触发条件很隐蔽：只有当响应体大于管道缓冲
# （约 64KB）、且命中点落在缓冲之内时才会发作。结果条数一多就中，
# 寥寥几条时一切正常，所以它是一颗随数据量引爆的哑雷。
# here-string 由 bash 用临时文件实现，grep 读到的是普通文件，不存在写端被打断的问题。
contains() {
  if grep -qF -- "$3" <<<"$2"; then ok "$1"; else bad "$1（未找到 $3）"; fi
}

missing() {
  if grep -qF -- "$3" <<<"$2"; then bad "$1（不该出现 $3）"; else ok "$1"; fi
}

# curl_get <url> [额外参数...] —— 自动附带鉴权头
curl_get() {
  local target="$1"; shift
  if [ -n "$TOKEN" ]; then
    curl -s --max-time 30 -H "X-Auth-Token: $TOKEN" "$@" "$target"
  else
    curl -s --max-time 30 "$@" "$target"
  fi
}

status_of()  { curl_get "$1" -o /dev/null -w '%{http_code}'; }
body_of()    { curl_get "$1"; }
headers_of() { curl_get "$1" -D - -o /dev/null; }

# 不带头部的裸请求，用于验证鉴权本身
raw_status() { curl -s --max-time 30 -o /dev/null -w '%{http_code}' "$1"; }

printf '\n\033[1mAvdbAsst 冒烟测试\033[0m  →  %s\n\n' "$BASE_URL"

# ---------------------------------------------------------------- 服务存活
printf '\033[1m[1] 服务存活\033[0m\n'
check "GET /healthz" "$(status_of "$BASE_URL/healthz")" "200"
contains "健康检查包含 status:ok" "$(body_of "$BASE_URL/healthz")" '"status":"ok"'
# 容器 HEALTHCHECK 依赖这条：配置了访问口令时也必须免鉴权
check "healthz 在启用口令后仍免鉴权" "$(raw_status "$BASE_URL/healthz")" "200"

# ---------------------------------------------------------------- 静态资源
printf '\n\033[1m[2] 静态资源（内嵌资源是否正确挂载）\033[0m\n'
check "GET /static/app.css"     "$(status_of "$BASE_URL/static/app.css")" "200"
check "GET /static/app.js"      "$(status_of "$BASE_URL/static/app.js")" "200"
check "GET /static/settings.js" "$(status_of "$BASE_URL/static/settings.js")" "200"
check "GET /favicon.svg"        "$(status_of "$BASE_URL/favicon.svg")" "200"

# ---------------------------------------------------------------- OpenSearch
printf '\n\033[1m[3] OpenSearch 描述文档\033[0m\n'
os_body="$(body_of "$BASE_URL/opensearch.xml")"
check "GET /opensearch.xml" "$(status_of "$BASE_URL/opensearch.xml")" "200"
contains "包含 searchTerms 占位符" "$os_body" '{searchTerms}'
contains "包含 /s?q= 路径"        "$os_body" '/s?q={searchTerms}'

# ---------------------------------------------------------------- 页面
printf '\n\033[1m[4] 页面渲染\033[0m\n'
home="$(body_of "$BASE_URL/")"
contains "首页含搜索表单" "$home" 'action="/s"'
contains "首页含设置入口" "$home" 'href="/settings"'

settings="$(body_of "$BASE_URL/settings")"
check "GET /settings" "$(status_of "$BASE_URL/settings")" "200"
contains "设置页含上游地址字段"   "$settings" 'name="api_base_url"'
contains "设置页含 API Key 字段"  "$settings" 'name="api_key"'
contains "设置页含下载器字段"     "$settings" 'name="default_downloader"'
contains "设置页含保存路径字段"   "$settings" 'name="default_save_path"'
contains "设置页含每页条数字段"   "$settings" 'name="page_size"'
contains "设置页含访问口令字段"   "$settings" 'name="access_token"'
contains "设置页含测试连接按钮"   "$settings" 'id="btn-test"'
contains "设置页含检测按钮"     "$settings" 'id="btn-detect"'
# 已下线的两个高级项不得再出现在页面上
missing  "设置页已下线请求超时项" "$settings" 'name="timeout_seconds"'
missing  "设置页已下线结果上限项" "$settings" 'name="max_results"'
# 只断言文件名，不断言分隔符：Windows 上是 \config.json，Linux 上是 /config.json
contains "设置页显示配置文件路径" "$settings" 'config.json'

# ---------------------------------------------------------------- 搜索
printf '\n\033[1m[5] 搜索链路\033[0m\n'
search_page="$(body_of "$BASE_URL/s?q=$QUERY")"
check "GET /s?q=$QUERY" "$(status_of "$BASE_URL/s?q=$QUERY")" "200"

if grep -q 'class="alert' <<<"$search_page"; then
  note "搜索返回了错误页（上游未配置或不可达），跳过结果断言"
  printf '        提示：打开 %s/settings 检查上游地址与 API Key\n' "$BASE_URL"
else
  contains "结果页含结果容器"   "$search_page" 'id="results"'
  contains "结果页为列表容器"   "$search_page" 'class="results"'
  contains "结果项为列表行"     "$search_page" 'class="row"'
  # 产品要求：只显示文字列表，不加载海报图
  missing  "结果页不含任何图片" "$search_page" '<img'
  contains "结果页含筛选工具条" "$search_page" 'class="toolbar"'
  contains "结果页含标题过滤框" "$search_page" 'id="f-filter"'
  # 筛选条件必须能在服务端生效：用一个不可能命中的站点筛一次，
  # 页面要明确说"筛剩下 0 条"，而不是伪装成"搜不到"。
  filtered="$(body_of "$BASE_URL/s?q=$QUERY&site=__no_such_site__")"
  contains "筛选条件在服务端生效" "$filtered" '当前筛选条件下没有结果'
  missing  "筛空后不再渲染结果行" "$filtered" 'class="row-check"'
  contains "筛空后仍可改筛选"     "$filtered" 'id="filter-form"'
  # 需求：结果行带序号、支持多选与批量操作
  contains "结果行带序号"       "$search_page" 'class="row-index"'
  contains "结果行带选择框"     "$search_page" 'class="row-check"'
  contains "结果页含批量操作条" "$search_page" 'id="batchbar"'
  contains "结果页含每页条数选择" "$search_page" 'name="page_size"'
  # 需求：这些字样不得再出现在页面上
  missing  "结果页不含「免费」字样" "$search_page" '免费'
  missing  "结果页不含「做种」字样" "$search_page" '做种'
  missing  "结果页不含产品名"     "$search_page" 'Avdb'

  api="$(body_of "$BASE_URL/api/search?q=$QUERY")"
  contains "JSON 接口返回 keyword"  "$api" '"keyword"'
  contains "JSON 接口返回 torrents" "$api" '"torrents"'
  contains "JSON 接口返回总命中数"  "$api" '"total"'
  contains "JSON 接口返回总页数"    "$api" '"total_pages"'
  contains "JSON 接口返回每页条数"  "$api" '"page_size"'

  count="$(printf '%s' "$api" | sed -n 's/.*"count":\([0-9]*\).*/\1/p' | head -1)"
  if [ -n "$count" ] && [ "$count" -gt 0 ] 2>/dev/null; then
    ok "搜索到 $count 条结果"
  else
    note "关键词 $QUERY 没有命中结果（可能上游数据里确实没有）"
  fi
fi

# ---------------------------------------------------------------- 参数兼容
printf '\n\033[1m[6] 搜索参数兼容性\033[0m\n'
for pattern in "/s?q=$QUERY" "/s?keyword=$QUERY" "/s?kw=$QUERY" \
               "/search?q=$QUERY" "/search?keyword=$QUERY" "/s/$QUERY" "/search/$QUERY"; do
  check "GET $pattern" "$(status_of "$BASE_URL$pattern")" "200"
done

# ---------------------------------------------------------------- 编码
printf '\n\033[1m[7] 中文与全角关键词（URL 编码）\033[0m\n'
check "GET /s?q=中文"     "$(status_of "$BASE_URL/s?q=%E4%B8%AD%E6%96%87")" "200"
check "GET /s?q=全角番号" "$(status_of "$BASE_URL/s?q=%EF%BC%A2%EF%BC%AF%EF%BC%A2%EF%BC%A2%EF%BC%8D%EF%BC%94%EF%BC%95%EF%BC%96")" "200"
check "GET /s?q=英文数字" "$(status_of "$BASE_URL/s?q=BOBB-456")" "200"
check "GET /api/search 无关键词应 400" "$(status_of "$BASE_URL/api/search")" "400"

# ---------------------------------------------------------------- 安全头
printf '\n\033[1m[8] 安全响应头\033[0m\n'
headers="$(headers_of "$BASE_URL/")"
contains "X-Content-Type-Options" "$headers" 'X-Content-Type-Options: nosniff'
contains "Content-Security-Policy" "$headers" 'Content-Security-Policy'
contains "CSP 限制脚本来源"       "$headers" "script-src 'self'"
missing  "CSP 不含 unsafe-inline" "$headers" 'unsafe-inline'
contains "搜索结果不缓存"         "$headers" 'Cache-Control: no-store'

# ---------------------------------------------------------------- 访问控制
printf '\n\033[1m[9] 访问控制\033[0m\n'
if [ -n "$TOKEN" ]; then
  check "无凭证访问 /s 应 401"     "$(raw_status "$BASE_URL/s?q=x")" "401"
  check "错误口令访问 /s 应 401"   "$(raw_status "$BASE_URL/s?q=x&token=definitely-wrong")" "401"
  check "正确口令（query）应 200"  "$(raw_status "$BASE_URL/s?q=x&token=$TOKEN")" "200"

  query_headers="$(curl -s --max-time 30 -D - -o /dev/null "$BASE_URL/s?q=x&token=$TOKEN")"
  contains "校验通过后下发 Cookie" "$query_headers" 'avdb_token='
  contains "Cookie 带 HttpOnly"    "$query_headers" 'HttpOnly'
  contains "Cookie 带 SameSite"    "$query_headers" 'SameSite'

  # 用 Cookie 访问应当无需再带口令
  cookie_status="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' -H "Cookie: avdb_token=$TOKEN" "$BASE_URL/s?q=x")"
  check "带 Cookie 访问应 200" "$cookie_status" "200"

  # 跨站提交设置必须被拒绝（CSRF 纵深防御）
  csrf_status="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
      -X POST -H "Origin: http://evil.example.com" \
      -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/x-www-form-urlencoded" \
      --data 'api_base_url=http://attacker:1' "$BASE_URL/settings")"
  check "跨站 POST /settings 应 403" "$csrf_status" "403"

  # 非法地址不得被接受
  bad_url_status="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
      -X POST -H "Origin: $BASE_URL" -H "X-Auth-Token: $TOKEN" \
      -H "Content-Type: application/x-www-form-urlencoded" \
      --data 'api_base_url=ftp://nope' "$BASE_URL/settings")"
  check "非法上游地址应 400" "$bad_url_status" "400"
else
  note "未提供 TOKEN，跳过鉴权相关断言（设 TOKEN=... 可启用）"
  check "未设口令时 /s 应开放" "$(raw_status "$BASE_URL/s?q=x")" "200"
fi

# ---------------------------------------------------------------- 下载收口
printf '\n\033[1m[10] 下载接口收口（只验证拒绝路径，不会真的提交下载）\033[0m\n'
# 提交下载是唯一的写操作：必须 POST，且必须同源。
# 下面三条断言全部落在"进入上游之前就被拒"，因此对生产实例执行也安全。
# 注意要带访问口令：鉴权中间件在路由之前，不带口令只会拿到 401，测不到方法/来源校验。
dl_get="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
    -H "X-Auth-Token: $TOKEN" "$BASE_URL/download?tid=1")"
check "GET /download 应 405（挡住 <img src> 触发）" "$dl_get" "405"

dl_no_tid="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
    -X POST -H "X-Auth-Token: $TOKEN" -H "Origin: $BASE_URL" \
    -H "Content-Type: application/x-www-form-urlencoded" --data '' "$BASE_URL/download")"
check "POST /download 缺 tid 应 400" "$dl_no_tid" "400"

dl_csrf="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
    -X POST -H "X-Auth-Token: $TOKEN" -H "Origin: http://evil.example.com" \
    -H "Referer: http://evil.example.com/attack.html" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data 'tid=1' "$BASE_URL/download")"
check "跨站 POST /download 应 403" "$dl_csrf" "403"

# 批量下载：同样只验证"进入上游之前就被拒"的路径
dl_batch_empty="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
    -X POST -H "X-Auth-Token: $TOKEN" -H "Origin: $BASE_URL" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data 'tids=' "$BASE_URL/download/batch")"
check "POST /download/batch 空 tids 应 400" "$dl_batch_empty" "400"

dl_batch_csrf="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
    -X POST -H "X-Auth-Token: $TOKEN" -H "Origin: http://evil.example.com" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data 'tids=1,2' "$BASE_URL/download/batch")"
check "跨站 POST /download/batch 应 403" "$dl_batch_csrf" "403"

# 单批上限 100：给 101 个合法 tid 应被拒，且这一步发生在解析下载目标之前，不会触达上游
oversize="$(seq 1 101 | tr '\n' ',' | sed 's/,$//')"
dl_batch_over="$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
    -X POST -H "X-Auth-Token: $TOKEN" -H "Origin: $BASE_URL" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data "tids=$oversize" "$BASE_URL/download/batch")"
check "POST /download/batch 超 100 条应 400" "$dl_batch_over" "400"

# ---------------------------------------------------------------- 汇总
printf '\n\033[1m结果\033[0m：\033[32m%d 通过\033[0m' "$pass"
[ "$skip" -gt 0 ] && printf '，\033[33m%d 跳过\033[0m' "$skip"
[ "$fail" -gt 0 ] && printf '，\033[31m%d 失败\033[0m' "$fail"
printf '\n\n'

[ "$fail" -eq 0 ]
