#!/bin/sh
# ---------------------------------------------------------------------------
# 入口脚本，只解决一件事：让「目录持久化」在任何属主情况下都能用。
#
# 问题：宿主机挂载进来的目录（NAS、群晖、Portainer 新建的目录）属主往往
# 不是容器内的 avdb(1000)。如果直接以 avdb 运行，写配置就会 permission denied，
# 表现为"页面上改了保存不了"。反过来一刀切用 root 跑又不必要地放大了权限。
#
# 做法：以 root 启动时先把配置目录的属主对齐，再降权到 avdb 执行。
# 如果使用者通过 `user:` 显式指定了非 root，则跳过对齐、直接运行。
# ---------------------------------------------------------------------------
set -e

CONFIG_DIR="${AVDB_CONFIG_DIR:-/data}"

if [ "$(id -u)" = "0" ]; then
    # 挂载为只读时 chown 会失败，此时不要中断启动，留给程序自己去报错。
    mkdir -p "$CONFIG_DIR" 2>/dev/null || true
    chown -R avdb:avdb "$CONFIG_DIR" 2>/dev/null \
        || echo "[entrypoint] 警告：无法修改 $CONFIG_DIR 属主，若保存配置失败请检查挂载权限" >&2
    exec su-exec avdb:avdb "$@"
fi

exec "$@"
