#!/usr/bin/env bash
# Multica 服务管理 — 统一走 systemd（用户级单元），行为对齐 centos 的 systemctl。
#
#   ./multica.sh status    查看所有服务状态
#   ./multica.sh start     启动后端 + 前端
#   ./multica.sh stop      停止
#   ./multica.sh restart   重启
#   ./multica.sh logs      跟随后端日志（Ctrl-C 退出）
#   ./multica.sh logs web  跟随前端日志
#   ./multica.sh enable    设置开机自启（含 PG 集群）
#   ./multica.sh disable   取消开机自启
#
# 单元位置：~/.config/systemd/user/multica-{backend,web}.service
# 源文件：  <repo>/.systemd/（改动后需重新 install）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNITS=(multica-backend multica-web multica-daemon)
UNIT_DIR="$HOME/.config/systemd/user"

ctl() { systemctl --user "$@"; }

install_units() {
  mkdir -p "$UNIT_DIR"
  cp "$ROOT"/.systemd/multica-*.service "$UNIT_DIR"/
  chmod 644 "$UNIT_DIR"/multica-*.service
  ctl daemon-reload
  echo "✓ 单元已同步到 $UNIT_DIR"
}

cmd_status() {
  printf "%-20s %-10s %-10s %s\n" SERVICE ACTIVE ENABLED PID
  for u in "${UNITS[@]}"; do
    printf "%-20s %-10s %-10s %s\n" "$u" \
      "$(ctl is-active "$u" 2>/dev/null || true)" \
      "$(ctl is-enabled "$u" 2>/dev/null || true)" \
      "$(ctl show -p MainPID --value "$u" 2>/dev/null || echo -)"
  done
  echo
  echo "PostgreSQL 17/multica (5433): $(systemctl is-active postgresql@17-multica 2>/dev/null) / $(systemctl is-enabled postgresql@17-multica 2>/dev/null)"
  echo "Redis:                        $(systemctl is-active redis-server 2>/dev/null) / $(systemctl is-enabled redis-server 2>/dev/null)"
  echo
  echo "端口:"
  ss -ltn 2>/dev/null | awk '$4 ~ /:(3000|10800|5433)$/ {printf "  %s\n", $4}'
}

case "${1:-status}" in
  status)  cmd_status ;;
  start)   ctl start "${UNITS[@]}"; sleep 3; cmd_status ;;
  stop)    ctl stop "${UNITS[@]}"; echo "✓ 已停止" ;;
  restart) ctl restart "${UNITS[@]}"; sleep 3; cmd_status ;;
  logs)
    unit="multica-${2:-backend}"
    exec journalctl --user -u "$unit" -f -n 100 --no-hostname
    ;;
  enable)
    ctl enable "${UNITS[@]}"
    # PG 集群是系统级单元，需要 sudo
    if ! systemctl is-enabled --quiet postgresql@17-multica 2>/dev/null; then
      echo "需要 sudo 启用 PostgreSQL 集群自启："
      echo "  sudo systemctl enable postgresql@17-multica.service"
    fi
    echo "✓ 已设置开机自启"
    ;;
  disable) ctl disable "${UNITS[@]}"; echo "✓ 已取消开机自启" ;;
  install) install_units ;;
  *)
    sed -n '2,14p' "$0"
    exit 1
    ;;
esac
