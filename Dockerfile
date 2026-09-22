# syntax=docker/dockerfile:1.7
# ---------------------------------------------------------------------------
# AvdbAsst 镜像
#   - 多阶段构建：编译期镜像不进最终产物
#   - 多架构：linux/amd64 + linux/arm64（NAS/树莓派常用 arm64）
#   - 配置持久化：/data 卷，可在页面里改地址与令牌并落盘
# ---------------------------------------------------------------------------

# ------------------------------------------------------------- 构建阶段
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

# TARGETOS / TARGETARCH 由 buildx 在跨架构构建时自动注入
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# 先只拷依赖清单，让依赖层能被独立缓存（本项目零外部依赖，但仍然保留这个习惯）
COPY go.mod ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 产出静态二进制，才能放进最小运行镜像
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/avdbasst .

# ------------------------------------------------------------- 运行阶段
FROM alpine:3.20

# ca-certificates —— 上游走 https 时必需
# tzdata          —— 日志时间戳
# su-exec         —— 入口脚本降权用
RUN apk add --no-cache ca-certificates tzdata su-exec \
 && addgroup -g 1000 -S avdb \
 && adduser  -u 1000 -S -G avdb -h /app avdb \
 && mkdir -p /app /data \
 && chown -R avdb:avdb /app /data

COPY --from=build /out/avdbasst /usr/local/bin/avdbasst
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# 配置持久化目录。容器重建、镜像升级都不会动这里的内容——
# 前提是把它挂载到宿主目录（见 docker-compose.yml）。
ENV AVDB_CONFIG_DIR=/data
VOLUME ["/data"]

# 监听地址固定为容器内 8080，对外端口用 -p 映射。
# 想改端口请改映射，不要改这里：容器内换端口只会让 HEALTHCHECK 和文档对不上。
ENV AVDB_ADDR=:8080
EXPOSE 8080

WORKDIR /app

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/usr/local/bin/avdbasst"]
