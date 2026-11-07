#!/usr/bin/env bash
# mutlica 一键编译 + 启动（源码最小化安装，无 Docker）
#
#   ./start.sh              编译（缺什么补什么）并启动后端 + 前端
#   ./start.sh --build-only 只编译，不启动
#   ./start.sh --start-only 只启动，不编译
#   ./start.sh --stop       停止服务
#
# 服务由 systemd 用户级单元托管（失败自重启 + 开机自启），本脚本只负责编译，
# 启动/停止委托给 multica.sh。单元源文件在 .systemd/。
#
# 依赖：Go 1.26.6 (go.mod 硬门槛) / Node 22 / pnpm / PostgreSQL 17 集群（5433）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

# ---------- 工具链（不走 hermit；系统 PATH 里没有 go） ----------
export PATH="$HOME/go/bin:$HOME/.nvm/versions/node/v22.23.1/bin:$PATH"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOTOOLCHAIN=local
GO_BIN="$(command -v go1.26.6 || command -v go || true)"
[ -n "$GO_BIN" ] || { echo "✗ 找不到 Go。需要 1.26.6：go install golang.org/dl/go1.26.6@latest && go1.26.6 download"; exit 1; }
command -v node >/dev/null || { echo "✗ 找不到 node（需要 22）"; exit 1; }
command -v pnpm >/dev/null || { echo "✗ 找不到 pnpm（需要 10.28.2）"; exit 1; }

BIN="$ROOT/.bin"
LOGS="$ROOT/.logs"
mkdir -p "$BIN" "$LOGS"

# ---------- 环境（.env 是现成的；local-env.sh 派生 PORT / FRONTEND_PORT 等） ----------
[ -f .env ] || { echo "✗ 缺 .env（可从 .env.example 复制）"; exit 1; }
set -a; . ./.env; set +a
. scripts/local-env.sh
BACKEND_PORT="${PORT:-10800}"
FRONTEND_PORT="${FRONTEND_PORT:-3000}"

do_stop() {
  # 服务归 systemd 管，别再按 pid 文件 kill —— 会被 Restart=always 拉回来。
  ./multica.sh stop
}

do_build() {
  echo "==> [1/3] Go 后端编译（首次约 4-7 分钟）"
  ( cd server && "$GO_BIN" build -o "$BIN/multica-server" ./cmd/server \
              && "$GO_BIN" build -o "$BIN/multica-migrate" ./cmd/migrate )
  echo "    ✓ $(du -h "$BIN/multica-server" | cut -f1) multica-server / $(du -h "$BIN/multica-migrate" | cut -f1) multica-migrate"

  echo "==> [2/3] 前端依赖"
  if [ ! -d node_modules ]; then pnpm install; else echo "    ✓ node_modules 已存在，跳过"; fi

  echo "==> [3/3] 前端生产构建（约 5-10 分钟，需 4G 堆）"
  ( cd apps/web && NODE_OPTIONS="--max-old-space-size=4096" NODE_ENV=production npx next build --webpack )
  echo "    ✓ .next 构建完成"
}

do_migrate() {
  echo "==> 数据库迁移（PG 17 @ 127.0.0.1:${POSTGRES_PORT}）"
  "$BIN/multica-migrate" up 2>&1 | tail -5
}

do_start() {
  # 生产模式硬约束：单元里跑 next start（预编译产物），不启 next dev / --turbo 热加载。
  # 启动、失败自重启、开机自启全部交给 systemd，这里只触发并做就绪探测。
  ./multica.sh start

  for i in $(seq 1 40); do
    b="$(curl -s -m2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$BACKEND_PORT/api/config" 2>/dev/null || echo 000)"
    f="$(curl -s -m2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$FRONTEND_PORT/" 2>/dev/null || echo 000)"
    if [ "$b" = "200" ] && [ "$f" = "200" ]; then
      echo ""
      echo "✓ multica 已就绪"
      echo "  前端  http://localhost:$FRONTEND_PORT"
      echo "  后端  http://localhost:$BACKEND_PORT"
      echo "  日志  ./multica.sh logs [backend|web]"
      echo "  停止  ./start.sh --stop"
      return 0
    fi
    sleep 1
  done
  echo "⚠ 启动未在 40s 内就绪（backend=$b frontend=$f），看日志："
  journalctl --user -u multica-backend -u multica-web -n 30 --no-pager
  return 1
}

case "${1:---all}" in
  --build-only) do_build ;;
  --start-only) do_migrate; do_start ;;
  --stop)       do_stop ;;
  --all|"")     do_stop; do_build; do_migrate; do_start ;;
  -h|--help)    sed -n '2,10p' "$0" ;;
  *) echo "未知参数: $1"; sed -n '2,10p' "$0"; exit 1 ;;
esac
