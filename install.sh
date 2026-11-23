#!/usr/bin/env bash
# relay-retry-proxy 一键安装脚本（Linux）
# 用法示例：
#   sudo ./install.sh --upstream http://10.0.0.5:4000
#   sudo ./install.sh -u http://10.0.0.5:4000 -l 0.0.0.0:8443 -a 6
#   ./install.sh --prefix /tmp/inst --no-systemd --dry-run   # 预演
set -euo pipefail

APP=relay-retry-proxy
VERSION=1.0.0

UPSTREAM="http://127.0.0.1:4000"
LISTEN="127.0.0.1:8443"
ATTEMPTS=5
BASE_DELAY="800ms"
MAX_DELAY="15s"
HEAD_TIMEOUT="90s"
DIAL_TIMEOUT="10s"
MAX_BODY_MB=64
HEDGE_AFTER="5s"
ADMIN_LISTEN="127.0.0.1:9090"

PREFIX="/usr/local"
SYSTEMD=1
DRY=0
FORCE=0
UNINSTALL=0

green() { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
red() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
die() { red "错误：$*"; exit 1; }
run() {
  if [ "$DRY" = 1 ]; then
    printf '  [dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

usage() {
  cat <<EOF
$APP 安装脚本 v$VERSION

  -u, --upstream URL      上游 base URL（默认 $UPSTREAM）
                          可写多个用逗号分隔做故障切换
  -l, --listen ADDR       监听地址（默认 $LISTEN）
                          本机自用 127.0.0.1:8443；给局域网用 0.0.0.0:8443
  -a, --attempts N        最大尝试次数（默认 $ATTEMPTS）
      --hedge-after D     对冲间隔：上游这么久没给响应头就并行再发（默认 $HEDGE_AFTER，0=关闭）
      --admin-listen A    WebUI 监听地址，逗号分隔可多个（默认 $ADMIN_LISTEN，0=关闭）
      --base-delay D      首次重试间隔（默认 $BASE_DELAY）
      --max-delay D       间隔上限（默认 $MAX_DELAY）
      --head-timeout D    等响应头上限（默认 $HEAD_TIMEOUT）
      --dial-timeout D    建连超时（默认 $DIAL_TIMEOUT）
      --prefix PATH       安装前缀（默认 $PREFIX，测试可用 /tmp/xxx）
      --no-systemd        不写 systemd 单元
      --dry-run           只打印将执行的动作
      --force             覆盖已存在的配置文件
      --uninstall         卸载（保留日志）
  -h, --help              显示本帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    -u|--upstream) UPSTREAM="${2:?}"; shift 2 ;;
    -l|--listen) LISTEN="${2:?}"; shift 2 ;;
    -a|--attempts) ATTEMPTS="${2:?}"; shift 2 ;;
    --hedge-after) HEDGE_AFTER="${2:?}"; shift 2 ;;
    --admin-listen) ADMIN_LISTEN="${2:?}"; shift 2 ;;
    --base-delay) BASE_DELAY="${2:?}"; shift 2 ;;
    --max-delay) MAX_DELAY="${2:?}"; shift 2 ;;
    --head-timeout) HEAD_TIMEOUT="${2:?}"; shift 2 ;;
    --dial-timeout) DIAL_TIMEOUT="${2:?}"; shift 2 ;;
    --prefix) PREFIX="${2:?}"; shift 2 ;;
    --no-systemd) SYSTEMD=0; shift ;;
    --dry-run) DRY=1; shift ;;
    --force) FORCE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "未知参数 $1（用 -h 看帮助）" ;;
  esac
done

[ "$(id -u)" = 0 ] || [ "$DRY" = 1 ] || die "请用 root 运行（sudo ./install.sh ...）"

BIN_DIR="$PREFIX/bin"
ETC_DIR="${RELAY_PROXY_ETC:-/etc/$APP}"
ENV_FILE="$ETC_DIR/env"
UNIT="${RELAY_PROXY_UNIT:-/etc/systemd/system/$APP.service}"
LOG_DIR="${RELAY_PROXY_LOG:-/var/log/$APP}"

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "不支持的架构 $(uname -m)，本包只含 amd64 / arm64" ;;
  esac
}

pick_binary() {
  local arch; arch=$(detect_arch)
  local cand
  for cand in "$(dirname "$0")/$APP-linux-$arch" "$(dirname "$0")/$APP-linux-$arch-static"; do
    [ -f "$cand" ] && { echo "$cand"; return 0; }
  done
  die "找不到 $APP-linux-$arch 二进制（包是否完整？）"
}

if [ "$UNINSTALL" = 1 ]; then
  yellow "卸载 $APP"
  if [ "$SYSTEMD" = 1 ] && command -v systemctl >/dev/null 2>&1; then
    run systemctl stop "$APP" || true
    run systemctl disable "$APP" || true
    run rm -f "$UNIT"
    run systemctl daemon-reload || true
  fi
  run rm -f "$BIN_DIR/$APP"
  yellow "配置保留在 $ENV_FILE，日志保留在 $LOG_DIR；要彻底清掉：rm -rf $ETC_DIR $LOG_DIR"
  green "卸载完成"
  exit 0
fi

BIN_SRC="$(pick_binary)"
ARCH="$(detect_arch)"
yellow "==> 安装 $APP v$VERSION ($ARCH) 到 $PREFIX"

run mkdir -p "$BIN_DIR"
run install -m 0755 "$BIN_SRC" "$BIN_DIR/$APP"
green "  二进制：$BIN_DIR/$APP"

run mkdir -p "$ETC_DIR" "$LOG_DIR"

if [ -f "$ENV_FILE" ] && [ "$FORCE" != 1 ]; then
  yellow "  配置已存在，保留原文件：$ENV_FILE（要覆盖加 --force）"
else
  if [ "$DRY" = 1 ]; then
    printf '  [dry-run] 写入 %s\n' "$ENV_FILE"
  else
    cat > "$ENV_FILE" <<EOF
# $APP 配置 — 由 install.sh 生成 $(date '+%F %T')
# 修改后：systemctl restart $APP
LISTEN=$LISTEN
UPSTREAM=$UPSTREAM
MAX_ATTEMPTS=$ATTEMPTS
BASE_DELAY=$BASE_DELAY
MAX_DELAY=$MAX_DELAY
HEAD_TIMEOUT=$HEAD_TIMEOUT
DIAL_TIMEOUT=$DIAL_TIMEOUT
HEDGE_AFTER=$HEDGE_AFTER
ADMIN_LISTEN=$ADMIN_LISTEN
MAX_BODY_MB=$MAX_BODY_MB
LOG=$LOG_DIR/proxy.log
EOF
    chmod 0644 "$ENV_FILE"
  fi
  green "  配置：$ENV_FILE"
fi

if [ "$SYSTEMD" = 1 ]; then
  if ! command -v systemctl >/dev/null 2>&1; then
    yellow "  系统没有 systemd，跳过服务注册；手动启动：$BIN_DIR/$APP -config $ENV_FILE"
  else
    if [ "$DRY" = 1 ]; then
      printf '  [dry-run] 写入 %s 并 enable --now\n' "$UNIT"
    else
      cat > "$UNIT" <<EOF
[Unit]
Description=$APP — OpenAI-compatible retry proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$BIN_DIR/$APP -listen \${LISTEN} -upstream \${UPSTREAM} \\
  -max-attempts \${MAX_ATTEMPTS} -base-delay \${BASE_DELAY} -max-delay \${MAX_DELAY} \\
  -head-timeout \${HEAD_TIMEOUT} -dial-timeout \${DIAL_TIMEOUT} \\
  -hedge-after \${HEDGE_AFTER} -admin-listen \${ADMIN_LISTEN} \\
  -max-body-mb \${MAX_BODY_MB} -log \${LOG}
Restart=always
RestartSec=2
LimitNOFILE=65535
NoNewPrivileges=true
PrivateTmp=true
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
      cat > "/etc/logrotate.d/$APP" <<EOF
$LOG_DIR/*.log {
    weekly
    rotate 4
    compress
    missingok
    notifempty
    copytruncate
}
EOF
      chmod 0644 "$UNIT" "/etc/logrotate.d/$APP"
    fi
    run systemctl daemon-reload
    run systemctl enable "$APP"
    run systemctl restart "$APP"
    green "  systemd：$UNIT（已 enable）"
  fi
fi

if [ "$DRY" = 0 ] && command -v systemctl >/dev/null 2>&1 && [ "$SYSTEMD" = 1 ]; then
  sleep 1
  if systemctl is-active --quiet "$APP"; then
    green "==> 服务运行中"
  else
    red "服务没起来，检查：journalctl -u $APP -n 50 --no-pager"
    exit 1
  fi
  PORT="${LISTEN##*:}"
  HOST="${LISTEN%:*}"
  [ "$HOST" = "0.0.0.0" ] && HOST=127.0.0.1
  if command -v curl >/dev/null 2>&1; then
    if curl -fsS -m 5 "http://$HOST:$PORT/__health" >/dev/null 2>&1; then
      green "==> 健康检查通过：http://$HOST:$PORT/__health"
    else
      yellow "健康检查未通过，端口 $PORT 可能未监听：journalctl -u $APP -n 30 --no-pager"
    fi
  fi
fi

cat <<EOF

$(green "安装完成")
  配置：$ENV_FILE
  日志：$LOG_DIR/proxy.log
  常用：
    systemctl status  $APP
    systemctl restart $APP
    journalctl -u $APP -n 50 --no-pager

  客户端把 Base URL 指过来：http://<本机地址>:${LISTEN##*:}/v1
  WebUI 看板：http://<本机地址>:9090/ui  （只读，不含代理转发）
EOF
