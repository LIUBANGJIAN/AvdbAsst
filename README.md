# AvdbAsst

把 Avdb 的资源检索包成一个可以直接被浏览器「自定义搜索引擎」调用的网页。

配好之后，在任意页面选中一段文字 → 右键 → 用本站搜索，就能直接落到结果页。
典型用法：看到番号，选中，右键搜。

```
https://你的地址/s?q=BOBB-456
```

- 纯 Go 标准库实现，**零第三方依赖**
- 单文件二进制，前端资源全部内嵌
- 搜索、下载器提交、上游地址与令牌 **全部可在网页里配置**
- 配置写入挂载目录，**容器重建不丢**
- 推送到 GitHub 自动构建 amd64 / arm64 双架构镜像并推送到 Docker Hub

---

## 目录

- [快速开始](#快速开始)
- [目录持久化](#目录持久化)
- [页面配置](#页面配置)
- [浏览器右键搜索](#浏览器右键搜索)
- [访问控制](#访问控制)
- [环境变量](#环境变量)
- [HTTP 接口](#http-接口)
- [自动化构建与 Docker Hub](#自动化构建与-docker-hub)
- [从源码构建](#从源码构建)
- [常见问题](#常见问题)
- [项目结构](#项目结构)

---

## 快速开始

### 方式一：Docker Compose（推荐）

```bash
git clone https://github.com/LIUBANGJIAN/AvdbAsst.git
cd AvdbAsst
mkdir -p data
docker compose up -d
```

然后打开 `http://<你的机器IP>:8080/settings`，填入上游 Avdb 地址和 API Key，保存即可。

### 方式二：docker run

```bash
docker run -d \
  --name avdbasst \
  --restart unless-stopped \
  -p 8080:8080 \
  -v "$(pwd)/data:/data" \
  -e TZ=Asia/Shanghai \
  liubangjian/avdbasst:latest
```

### 方式三：直接跑二进制

```bash
go build -o avdbasst .
AVDB_CONFIG_DIR=./data ./avdbasst
# 浏览器打开 http://localhost:8080/settings
```

---

## 目录持久化

配置（含上游地址、API Key、访问口令）保存在 **`$AVDB_CONFIG_DIR/config.json`**：

| 环境 | 配置目录 |
| --- | --- |
| 容器内 | `/data`（Dockerfile 已设好） |
| 本地运行 | `.`（当前目录），可用 `AVDB_CONFIG_DIR` 改 |

**要让它真正持久，必须把宿主目录挂载到 `/data`：**

```yaml
volumes:
  - ./data:/data
```

没挂载会怎样：容器照常运行、也能在页面里改配置，但容器一重建配置就没了。
设置页面会显示配置目录路径与「可写」状态，方便自查。

写入是**原子**的：先写临时文件、`fsync`、再 `rename`。
中途断电最多丢掉最后一次修改，不会留下半截 JSON 让下次启动直接失败。
文件权限 `0600`（里面是明文令牌）。

---

## 页面配置

打开 `/settings` 即可配置：

| 项 | 说明 |
| --- | --- |
| 上游地址 | Avdb 实例地址，如 `http://192.168.1.20:8000` |
| API Key | 上游 `X-API-Key`，在 Avdb 的「控制中心 → 访问令牌」创建 |
| 下载器 | 提交离线下载用的下载器标识，默认 `115` |
| 保存路径 | 提交离线下载时的目录，留空用上游默认 |
| 访问口令 | 给本站自己加的访问控制，见下节 |
| 请求超时 / 结果上限 | 高级项 |

点「保存设置」即时生效，**不需要重启容器**。

页面上还有个「测试连接」按钮，保存之前就能确认地址和令牌能不能用。

### 密钥为什么是掩码

API Key 与访问口令在页面上**只显示掩码**（如 `fpOJ••••••••1BZ`），输入框永远不回显明文。
需要修改时直接粘贴新值；留空表示不修改；勾选「清除」才会清空。

这样做是刻意的：Avdb 官方文档明确要求访问令牌不得进入前端，
而页面 HTML 会经过浏览器缓存、代理日志和截图。掩码既能让你确认"已配置"，
又不会在链路上留下完整凭证。

### 配置优先级

```
内置默认值  <  环境变量  <  配置文件（页面保存的值）
```

配置文件排在最后，因为那是你自己在页面上显式改过的内容——
如果反过来，就会出现"改完刷新又变回去了"这种最难排查的问题。
环境变量在这里的角色是**首次启动的初值**（容器部署时用来预置），不是最终裁决者。

> **例外**：监听地址（`AVDB_ADDR`）与配置目录（`AVDB_CONFIG_DIR`）属于部署参数，
> 永远由环境变量决定，不受配置文件影响——改它们需要重启，让它们在页面上"看起来能改"
> 只会制造误解。

想用环境变量重新接管某个字段，删掉 `data/config.json` 里对应的键即可。

---

## 浏览器右键搜索

服务端同时接受这些写法，随便用哪种：

| 模板 | 用在哪儿 |
| --- | --- |
| `/s?q=%s` | Chrome / Edge / Firefox 的「自定义搜索引擎」 |
| `/s?q={searchTerms}` | OpenSearch 规范 |
| `/s?q={{ q }}` | 各类右键菜单插件的自定义模板 |
| `/s/{关键词}` | 路径式，部分书签工具更顺手 |

参数名除 `q` 外，还兼容 `keyword`、`kw`、`wd`、`query`、`s`。

### 方式一：一键添加（OpenSearch）

访问 `http://<你的地址>:8080/`，点首屏的「添加为搜索引擎」。
浏览器会自动识别并提示加入搜索引擎列表。

### 方式二：Chrome / Edge 手动添加

1. 打开 `chrome://settings/searchEngines`
2. 找到「网站搜索」→「添加」
3. 填入：
   - 名称：`Avdb`
   - 快捷字词：`av`（可选，之后在地址栏敲 `av ` + 空格即可搜索）
   - 网址：`http://<你的地址>:8080/s?q=%s`
4. 保存

之后地址栏输入 `av 关键词` 直接搜；选中文字后右键「使用 Avdb 搜索」也可（取决于浏览器/插件）。

### 方式三：右键菜单插件

各类支持自定义搜索引擎的右键插件，把请求地址填成：

```
http://<你的地址>:8080/s?q={{ q }}
```

插件自己的占位符写法照它的要求写，服务端三种都认。

### 关键词处理

- **中文、英文、日文假名、数字、混合写法**都支持，走标准 URL 编码
- **全角自动转半角**：中文输入法下很容易打出全角番号（`ＢＯＢＢ－４５６`），
  直接送上游一条都搜不到，服务端会先折成半角 `BOBB-456` 再查询
- 连续空格折叠、首尾空白裁掉、控制字符剔除
- 超长关键词截断到 200 字符

---

## 访问控制

默认**不启用**。设置「访问口令」后，所有页面都需要先通过校验：

```
http://<你的地址>:8080/s?q=关键词&token=你的口令
```

首次带 `token` 访问会写入 Cookie（30 天），之后不用重复携带。
也可以用请求头 `X-Auth-Token`。

`/healthz` 始终免鉴权，否则容器编排无法探活。

> **注意**：本站代理了「提交下载」这一会改变状态的操作，而且持有你的 Avdb 令牌。
> 如果打算把它暴露到公网，**务必**设置访问口令，并建议在反向代理上再加一层认证。
> 只在内网使用时，不设也可以。

---

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AVDB_API_BASE_URL` | `http://127.0.0.1:8999` | 上游地址（初值） |
| `AVDB_API_KEY` | 空 | 上游令牌（初值） |
| `AVDB_DOWNLOADER` | `115` | 下载器标识 |
| `AVDB_SAVE_PATH` | 空 | 下载保存路径 |
| `AVDB_ACCESS_TOKEN` | 空 | 本站访问口令 |
| `AVDB_TIMEOUT_SECONDS` | `20` | 上游请求超时（1–600） |
| `AVDB_MAX_RESULTS` | `500` | 单次搜索展示上限（1–5000） |
| `AVDB_ADDR` | `:8080` | 监听地址（部署参数，仅环境变量生效） |
| `AVDB_CONFIG_DIR` | `.` | 配置目录（容器内已是 `/data`） |
| `PORT` | — | `AVDB_ADDR` 未设时的备选 |
| `TZ` | — | 时区，影响日志时间戳 |

---

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/` | 首页；带 `?q=` 时直接出结果 |
| GET | `/s?q=关键词` | **搜索主入口**，浏览器搜索模板用这个 |
| GET | `/s/{关键词}` | 路径式搜索 |
| GET | `/search?keyword=关键词` | 兼容旧地址 |
| GET | `/api/search?q=关键词` | JSON 结果 |
| GET | `/download?tid=xxx` | 提交下载，返回 `{success, message}` |
| GET | `/settings` | 设置页面 |
| POST | `/settings` | 保存设置 |
| POST | `/api/settings/test` | 测试上游连通性 |
| GET | `/opensearch.xml` | OpenSearch 描述文档 |
| GET | `/healthz` | 健康检查 |
| GET | `/static/*`、`/favicon.svg` | 静态资源 |

### `GET /api/search` 响应示例

```json
{
  "keyword": "BOBB-456",
  "count": 2,
  "torrents": [
    {
      "id": 10080460000,
      "title": "BOBB-456 [BT](ABC) ...",
      "site": "x1080x",
      "section": "亚洲有码",
      "category": "",
      "size_mb": 5244,
      "post_time": "2026-02-22 20:13:14",
      "seeders": 66,
      "download_url": "magnet:?xt=urn:btih:...",
      "preview_image": "https://.../off_BOBB-456.jpg",
      "chinese": false,
      "uncensored": false,
      "hd": true,
      "uhd": false,
      "free": true
    }
  ],
  "sites": ["x1080x"],
  "sections": ["亚洲有码"],
  "categories": [],
  "truncated": false
}
```

失败时返回 `{"error": "..."}`，HTTP 状态码 `502`。

---

## 自动化构建与 Docker Hub

`.github/workflows/docker-publish.yml` 已经配好：

- **推送到 `main`** → 跑测试 → 构建 amd64/arm64 → 推送 `latest` + `sha-xxxxxxx`
- **打 `v1.2.3` 标签** → 额外推送 `1.2.3`、`1.2`、`1`
- **提 PR** → 只跑测试，不推镜像
- 支持手动触发（Actions 页面 → Run workflow）

### 首次使用前必须做两件事

**1. 在 GitHub 仓库里配置 Secrets**

`Settings → Secrets and variables → Actions → New repository secret`

| Secret | 值 |
| --- | --- |
| `DOCKERHUB_USERNAME` | 你的 Docker Hub 用户名 |
| `DOCKERHUB_TOKEN` | Docker Hub 的 Access Token（**不是**登录密码） |

Docker Hub Token 在 `hub.docker.com → Account Settings → Personal access tokens` 创建，
权限选 `Read & Write`。

**2. 确认镜像名**

workflow 里拼出来的是 `<DOCKERHUB_USERNAME>/avdbasst`（见 `env.IMAGE_NAME`）。
Docker Hub 的仓库名**必须全小写**，Docker 不接受大写仓库名。
如果你的用户名或想用的仓库名不一样，改 `IMAGE_NAME` 或直接用推送时自动创建的私有仓库。

配好之后，`git push` 就会自动出镜像：

```bash
docker pull <你的用户名>/avdbasst:latest
```

---

## 从源码构建

需要 Go 1.24 或更高版本。

```bash
go build -trimpath -o avdbasst .
```

### 测试

```bash
go vet ./...
go test ./...          # 全部用例
go test -race ./...    # 含竞态检测
go test -run TestSettings -v ./...   # 只跑设置相关
```

测试覆盖：配置装载与优先级、配置原子落盘与重载、密钥掩码、
上游响应解析（含超 int32 的 ID 与 `null` 字段）、关键词归一化（中文/全角/非法 UTF-8）、
筛选器生成顺序确定性、XSS 转义、路由等价性、访问口令、CSRF 防护、
下载结果解读、超时与上游故障降级。

### 交叉编译

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o avdbasst-arm64 .
```

---

## 常见问题

**搜索结果 401**

上游令牌不对或没填。去 `/settings` 点「测试连接」会直接告诉你原因。

**页面能开，但点「下载」失败**

检查 `下载器` 与 `保存路径`。Avdb 侧需要先配置好对应的下载器，
`115` 是上游的默认值。

**容器重启后配置回到默认值**

卷没挂上。确认 `docker compose` 里写了 `- ./data:/data`，
并且 `/settings` 页面上「配置目录」显示为可写。

**页面上保存配置提示失败**

配置目录不可写。用 `docker compose logs avdbasst` 看具体错误；
常见原因是宿主机目录属主不对。镜像入口脚本会在以 root 启动时
自动对齐 `/data` 属主，若仍然失败请检查挂载是否为只读。

**缩略图不显示**

很多资源站的图片禁止外链，这是上游图片服务器的限制，不影响搜索与下载。
页面会退化成占位块。

**日期和上游差一天**

不会。展示的日期直接取自上游返回的 `post_time`，不做任何时区换算。
鼠标悬停在日期上可以看到上游原始时间串。

**搜索很慢**

默认超时 20 秒。上游是本地服务的话可以在设置里调小到 5–10 秒，
失败能更快暴露。客户端偶发连接抖动会自动重试一次。

---

## 项目结构

```
.
├── main.go                      程序入口、路由装配、优雅退出
├── config.go                    配置装载（默认值 < 环境变量 < 文件）、原子落盘、密钥掩码
├── avdb.go                      上游客户端、数据结构、文本归一化
├── handlers.go                  HTTP 处理器、中间件、设置页面逻辑
├── assets.go                    前端资源内嵌与模板解析
├── web/
│   ├── index.html               首页模板（服务端直出，无 JS 也可用）
│   ├── settings.html            设置页模板
│   ├── favicon.svg
│   └── static/
│       ├── app.css
│       ├── app.js               筛选、排序、下载、复制
│       └── settings.js          测试连接
├── Dockerfile                   多阶段 + 多架构构建
├── docker-entrypoint.sh         卷属主对齐后降权运行
├── docker-compose.yml           部署示例（含目录持久化）
├── config.example.json          本地运行的配置样例
└── .github/workflows/
    └── docker-publish.yml       测试 + 构建 + 推送 Docker Hub
```

### 几个实现上的选择

- **前端零内联脚本**，全部走外部文件 + 事件委托，因此 CSP 可以收紧到
  `script-src 'self'`。上游标题属于外部不可信输入，一旦用 `innerHTML` 拼接
  就是存储型 XSS，所以所有数据都经模板转义后放进 `data-*` 属性。
- **搜索结果服务端直出**，浏览器禁用 JS、或插件只做"打开 URL"这一动作时，
  页面依然是完整的。
- **筛选下拉的顺序是显式排序的**。用 map 遍历生成会让同一份数据每次刷新顺序都不同，
  属于"同一输入两次结果不一致"的安静型故障，测试里连跑 200 次做回归。
- **协议白名单**。上游返回的 `download_url` / `preview_image` 只放行
  `magnet/http/https/ed2k/thunder/ftp`，避免上游数据被污染后把 `javascript:` 带进页面。
