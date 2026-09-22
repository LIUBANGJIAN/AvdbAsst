# AvdbAsst

把 Avdb 的资源检索包成一个可以直接被浏览器「自定义搜索引擎」调用的网页。

配好之后，在任意页面选中一段文字 → 右键 → 用本站搜索，就能直接落到结果页。
典型用法：看到番号，选中，右键搜。

```
https://你的地址/s?q=BOBB-456
```

- 纯 Go 标准库实现，**零第三方依赖**
- 单文件二进制，前端资源全部内嵌
- 结果以**列表**呈现，只加载文字信息、不拉取海报图，首屏更快
- 搜索、下载器提交、上游地址与令牌 **全部可在网页里配置**
- 下载器标识可**就地校验**（只读目录清单）；也能用登录凭据换 JWT 读出上游的下载器清单
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
| 下载器 | 提交离线下载用的下载器标识，**必填**，默认 `115`。不确定就点「校验下载器并列出目录」 |
| 保存路径 | 提交离线下载时的目录，留空即由上游决定 |
| 访问口令 | 给本站自己加的访问控制，见下节 |
| 请求超时 / 结果上限 | 高级项 |

点「保存设置」即时生效，**不需要重启容器**。

页面上还有两个探测按钮：**测试连接**（保存前确认地址和令牌能不能用）、
**校验下载器并列出目录**（确认下载器标识有效，并把它下面的目录带回来做下拉提示）。

### 下载器标识为什么不能留空

上游 `GET /api/v1/articles/download/manul` 的接口文档把 `downloader` 和 `save_path`
标成「选填」，但实际行为是**必填**：`downloader` 传空、或传一个上游不认识的值，
都会收到 `未找到下载器: xxx`。文档里那句"为空时继承全局订阅下载器"讲的是
`JavdbSubscriptionRuleForm`（订阅规则），跟 `download/manul` 不是一个接口。

所以本站的做法是：`downloader` 始终带值出场（内置默认 `115`，可在设置页改），
`save_path` 才允许留空。拿不准标识该填什么，用两个按钮任选其一：

- **校验下载器并列出目录**：`GET /api/v1/config/downloader/directories?downloader_id=…`，
  **只需要访问令牌**，返回该标识对应的目录清单，顺便验证标识本身有效；
- **从上游读取配置**：需要用户名 + 密码换临时 JWT，能读到完整的下载器清单。

两个入口的结果都只缓存「标识 + 显示名」这类不含凭据的部分，供输入框做下拉提示。

### 关于"登录上游读配置"

上游把「有哪些下载器」放在自己的配置里，读取接口是 `GET /api/v1/config/{key}`，
它**只接受登录 JWT**（用户名 + 密码换来的），访问令牌（`X-API-Key`）无权调用。
所以想自动列出下载器，只能让用户填一次登录凭据——这也是旧版脚本没做的事：
`_老脚本main.go` 要求你手工编辑 `config.json` 里的 `default_downloader` /
`default_save_path`，填错同样只会在提交下载时看到一句「未找到下载器」。

设置页的「从上游读取配置」面板走这条路，并且：

- 密码与 JWT **只在本次请求内使用**：不写入配置、不落盘、不记日志；
- 上游开了两步验证时，会就地展开「动态验证码」输入框，用 `otp_token` 续一次
  `POST /api/v1/users/login/2fa`；
- 有一个测试专门盯着这件事（`TestSettingsLoginNeverPersistsPassword`），
  把密码和 JWT 当成不该出现在配置文件里的字符串去扫描磁盘。

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
>
> 「提交下载」另有两道与口令无关的收口：**只接受 POST**，并且校验请求来源（同源）。
> 两者缺一不可——只用来源校验挡不住把 `referrerpolicy` 设成 `no-referrer` 的
> `<img src=".../download?tid=1">`，而只改方法不给来源校验则挡不住跨站表单提交。

---

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AVDB_API_BASE_URL` | `http://127.0.0.1:8999` | 上游地址（初值） |
| `AVDB_API_KEY` | 空 | 上游令牌（初值） |
| `AVDB_DOWNLOADER` | 空（归一后为 `115`） | 下载器标识；留空会用内置默认值 `115` |
| `AVDB_SAVE_PATH` | 空 | 下载保存路径，留空即由上游决定 |
| `AVDB_ACCESS_TOKEN` | 空 | 本站访问口令 |
| `AVDB_TIMEOUT_SECONDS` | `20` | 上游请求超时（1–600） |
| `AVDB_MAX_RESULTS` | `500` | 单次搜索展示上限（1–5000） |
| `AVDB_ADDR` | `:8080` | 监听地址（部署参数，仅环境变量生效） |
| `AVDB_CONFIG_DIR` | `.` | 配置目录（容器内已是 `/data`） |
| `AVDB_DISABLE_ORIGIN_CHECK` | 空 | 设为 `1` 关闭来源校验（仅用于反向代理无法回传原始主机时的应急，会削弱 CSRF 防护） |
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
| POST | `/download` | 提交下载（表单字段 `tid` 或查询串 `?tid=`），返回 `{success, message}` |
| GET | `/settings` | 设置页面 |
| POST | `/settings` | 保存设置 |
| POST | `/api/settings/test` | 测试上游连通性 |
| POST | `/api/settings/directories` | 用已有令牌校验下载器标识并列出其目录（表单：`downloader_id`） |
| POST | `/api/settings/login` | 用用户名+密码换 JWT 读取下载器清单（表单：`username`、`password`，第二步加 `otp_token` + `otp_code`） |
| GET | `/opensearch.xml` | OpenSearch 描述文档 |
| GET | `/healthz` | 健康检查 |
| GET | `/static/*`、`/favicon.svg` | 静态资源 |

三个 `POST /api/settings/*` 都要求同源
（`Origin`/`Host` 口径与 `POST /settings` 一致），且都**不写入任何配置**：
密码与 JWT 只活在这一个请求的栈上。

`POST /api/settings/login` 的响应里，`need_2fa: true` 表示当前凭据需要动态验证码，
把上游返回的 `otp_token` 和用户输入的 6 位码一起再发一次即可（`otp_token` 由服务端
在响应中带回，客户端不落存储，只在内存里传给下一步）。

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

先看提示原文，现在会照实转述上游的校验结果，照做即可：

- `未找到下载器：xxx`
  上游不认识这个下载器标识。去 `/settings` 点「校验下载器并列出目录」，
  或走「从上游读取配置」登录一次，把标识改成清单里的值。
- `上游参数校验失败（422）：查询参数 → save_path：Field required`
  说明上游要求显式传 `save_path`。本站无论如何都会发送该参数（空值也算发送），
  所以出现这条通常意味着上游版本行为又有变化——把提示发出来即可定位。
- `下载器未配置` / 类似的业务错误码
  上游侧确实什么都没配。去 Avdb 里先配好一个下载器。

**容器重启后配置回到默认值**

卷没挂上。确认 `docker compose` 里写了 `- ./data:/data`，
并且 `/settings` 页面上「配置目录」显示为可写。

**页面上保存配置提示失败**

配置目录不可写。用 `docker compose logs avdbasst` 看具体错误；
常见原因是宿主机目录属主不对。镜像入口脚本会在以 root 启动时
自动对齐 `/data` 属主，若仍然失败请检查挂载是否为只读。

**保存设置返回 403「跨站请求被拒绝」**

服务端会校验请求来源，防止别的网站借你的浏览器改配置。若本站部署在反向代理后面，
而代理没有把原始主机名回传给后端，就会出现「页面打得开、一保存就 403」——
后端看到的 `Host` 是内部地址，浏览器的 `Origin` 却是外部域名，两边对不上。

让代理回传原始主机即可，Nginx 示例：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Real-IP $remote_addr;
}
```

403 页面会直接列出实际收到的 `Origin` / `Referer` / `Host` / `X-Forwarded-Host`，
照着对比即可定位。若确认同源却仍被拦截，可临时设 `AVDB_DISABLE_ORIGIN_CHECK=1`
关闭该校验——注意这会同时关掉这一层 CSRF 纵深防御，不建议长期开启。

**想看到海报图**

结果页按设计**只显示文字列表**，不再加载海报：这类图片多为站点防盗链资源，
加载慢、失败率高，而列表场景真正用来判断的是番号、体积、做种与标签。
上游返回的 `preview_image` 仍会出现在 `/api/search` 的 JSON 里（已做协议白名单），
需要图片的自建前端可以直接取用。

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
├── upstream_auth.go             登录换 JWT、两步验证、读上游配置、下载器与目录探测
├── json_harvest.go              从上游配置里按白名单键名挑出下载器/目录（绝不整段回传）
├── handlers.go                  HTTP 处理器、中间件、设置页面逻辑
├── assets.go                    前端资源内嵌与模板解析
├── web/
│   ├── index.html               首页模板（服务端直出，无 JS 也可用）
│   ├── settings.html            设置页模板
│   ├── favicon.svg
│   └── static/
│       ├── app.css
│       ├── app.js               筛选、排序、下载、复制
│       └── settings.js          测试连接、校验下载器、登录上游
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
- **凭据的双向闸门**。登录探测带进来的是密码，探测出来的上游配置里可能躺着网盘
  Cookie——所以两个方向都设了闸：请求侧密码/JWT 不落盘、不进日志（有测试扫描
  磁盘上的配置文件来兜底），响应侧只按白名单键名摘取 `id`/`name`/`path` 这类字段，
  绝不把上游配置整段回传浏览器。
- **`老脚本main.go` 保留但从构建中排除**。它是最初手工跑通的版本，作为"上游接口该怎么调"
  的参考件留着；文件名前的下划线让 Go 工具链忽略它（它和主包重名 `main`），
  `.gitignore` 也已排除 `_老脚本*.go`。
