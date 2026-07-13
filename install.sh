#!/usr/bin/env bash
#
# GoWeb S3/OSS 目录浏览 一键部署脚本
#
# 用法：
#   sudo ./install.sh                 # 安装或升级（自动寻找/构建二进制）
#   sudo ./install.sh --port 9090     # 指定监听端口
#   sudo ./install.sh --binary FILE   # 指定已编译好的二进制文件
#   sudo ./install.sh uninstall       # 卸载（保留数据目录）
#   sudo ./install.sh uninstall --purge   # 卸载并删除数据目录
#
# 多租户（每个租户一个完全独立的实例）：
#   sudo ./install.sh --name tenant-a --port 9081    # 安装/升级租户实例
#   sudo ./install.sh uninstall --name tenant-a      # 卸载租户实例
#
# 默认安装位置：/opt/goweb，服务名 goweb；
# 指定 --name 后为 /opt/goweb-<name>，服务名 goweb-<name>，配置、密钥、
# 会话与审计日志完全独立。服务以 nobody 用户运行。
# 目标机无 systemd 时仅安装文件并给出手动启动命令。

set -euo pipefail

NAME=""
PORT=""
BINARY=""
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---- 输出辅助 ----
if [ -t 1 ]; then
    C_OK=$'\033[32m'; C_ERR=$'\033[31m'; C_WARN=$'\033[33m'; C_RST=$'\033[0m'
else
    C_OK=""; C_ERR=""; C_WARN=""; C_RST=""
fi
info() { echo "${C_OK}[OK]${C_RST} $*"; }
warn() { echo "${C_WARN}[!!]${C_RST} $*"; }
die()  { echo "${C_ERR}[错误]${C_RST} $*" >&2; exit 1; }

usage() { sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- 参数解析 ----
ACTION="install"
PURGE=0
while [ $# -gt 0 ]; do
    case "$1" in
        uninstall)      ACTION="uninstall" ;;
        --name)         NAME="${2:?--name 需要参数}"; shift ;;
        --port)         PORT="${2:?--port 需要参数}"; shift ;;
        --binary)       BINARY="${2:?--binary 需要参数}"; shift ;;
        --purge)        PURGE=1 ;;
        -h|--help)      usage ;;
        *)              die "未知参数: $1（--help 查看用法）" ;;
    esac
    shift
done

[ "$(id -u)" -eq 0 ] || die "请以 root 运行：sudo $0 $*"

# ---- 实例定位（--name 为空时是默认单实例）----
if [ -n "$NAME" ]; then
    printf '%s' "$NAME" | grep -Eq '^[a-z0-9][a-z0-9-]*$' \
        || die "--name 只允许小写字母、数字和中划线（示例：tenant-a）"
    INSTALL_DIR="/opt/goweb-$NAME"
    SERVICE_NAME="goweb-$NAME"
    # 多实例并存，端口必须显式指定避免冲突
    if [ "$ACTION" = "install" ] && [ -z "$PORT" ]; then
        die "多实例安装必须用 --port 指定端口（避免与其他实例冲突）"
    fi
else
    INSTALL_DIR="/opt/goweb"
    SERVICE_NAME="goweb"
fi
PORT="${PORT:-8080}"
DATA_DIR="$INSTALL_DIR/data"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

HAS_SYSTEMD=0
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    HAS_SYSTEMD=1
fi

# ---- 卸载 ----
do_uninstall() {
    if [ "$HAS_SYSTEMD" -eq 1 ] && [ -f "$SERVICE_FILE" ]; then
        systemctl disable --now "$SERVICE_NAME" 2>/dev/null || true
        rm -f "$SERVICE_FILE"
        systemctl daemon-reload
        info "已停止并移除 systemd 服务"
    fi
    rm -f "$INSTALL_DIR/goweb"
    info "已删除二进制 $INSTALL_DIR/goweb"
    if [ "$PURGE" -eq 1 ]; then
        rm -rf "$DATA_DIR"
        rmdir "$INSTALL_DIR" 2>/dev/null || true
        warn "已删除数据目录 $DATA_DIR（含配置与审计日志）"
    else
        info "数据目录已保留: $DATA_DIR（如需一并删除请加 --purge）"
    fi
    info "卸载完成"
    exit 0
}
[ "$ACTION" = "uninstall" ] && do_uninstall

# ---- 定位或构建二进制 ----
find_binary() {
    # 1) --binary 显式指定
    if [ -n "$BINARY" ]; then
        [ -f "$BINARY" ] || die "指定的二进制不存在: $BINARY"
        return
    fi
    # 2) 脚本同目录下的现成二进制（离线拷贝场景）
    local arch cand
    case "$(uname -m)" in
        x86_64)          arch="amd64" ;;
        aarch64|arm64)   arch="arm64" ;;
        *)               arch="" ;;
    esac
    for cand in "$SCRIPT_DIR/goweb-linux-$arch" "$SCRIPT_DIR/goweb"; do
        if [ -n "$arch" ] && [ -f "$cand" ] && [ -x "$cand" ]; then
            BINARY="$cand"
            info "使用现成二进制: $BINARY"
            return
        fi
    done
    # 3) 位于源码仓库且有 Go 工具链则现场构建
    if [ -f "$SCRIPT_DIR/go.mod" ] && command -v go >/dev/null 2>&1; then
        info "未找到现成二进制，使用 $(go version | awk '{print $3}') 现场构建…"
        (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/goweb-build .) \
            || die "构建失败"
        BINARY="/tmp/goweb-build"
        return
    fi
    die "找不到可用的二进制。请任选其一：
  1. 把编译好的 goweb（或 goweb-linux-amd64/arm64）放到脚本同目录
  2. 用 --binary FILE 指定二进制路径
  3. 在源码目录中运行本脚本，且目标机装有 Go 1.24+"
}

# ---- 安装 ----
find_binary

UPGRADING=0
if [ -f "$INSTALL_DIR/goweb" ]; then
    UPGRADING=1
    if [ "$HAS_SYSTEMD" -eq 1 ]; then
        systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    fi
    cp -f "$INSTALL_DIR/goweb" "$INSTALL_DIR/goweb.bak"
    info "检测到已有安装，旧版本已备份为 goweb.bak"
fi

mkdir -p "$DATA_DIR"
install -m 0755 "$BINARY" "$INSTALL_DIR/goweb"
chown -R nobody "$DATA_DIR"
info "二进制与数据目录就绪: $INSTALL_DIR"

# SELinux（RHEL 系）：允许 systemd 执行非常规路径下的二进制
if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" = "Enforcing" ]; then
    chcon -t bin_t "$INSTALL_DIR/goweb" 2>/dev/null \
        && info "已设置 SELinux 上下文 (bin_t)" \
        || warn "SELinux 处于 Enforcing 且设置上下文失败，服务无法启动时请检查 audit 日志"
fi

if [ "$HAS_SYSTEMD" -ne 1 ]; then
    warn "未检测到 systemd，跳过服务注册。手动启动："
    echo "  GOWEB_LISTEN=:$PORT GOWEB_DATA_DIR=$DATA_DIR $INSTALL_DIR/goweb"
    exit 0
fi

cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=GoWeb S3/OSS 目录浏览
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nobody
WorkingDirectory=$INSTALL_DIR
Environment=GOWEB_LISTEN=:$PORT
Environment=GOWEB_DATA_DIR=$DATA_DIR
ExecStart=$INSTALL_DIR/goweb
Restart=on-failure
RestartSec=3

# 安全加固
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=$DATA_DIR
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME"

# 等待服务起来并做一次健康检查
sleep 1
for i in 1 2 3 4 5; do
    if systemctl is-active --quiet "$SERVICE_NAME"; then break; fi
    sleep 1
done
if ! systemctl is-active --quiet "$SERVICE_NAME"; then
    journalctl -u "$SERVICE_NAME" -n 20 --no-pager || true
    die "服务启动失败，请查看上方日志（journalctl -u $SERVICE_NAME）"
fi
if command -v curl >/dev/null 2>&1; then
    curl -sf -o /dev/null "http://127.0.0.1:$PORT/login" \
        && info "健康检查通过 (http://127.0.0.1:$PORT)" \
        || warn "服务已运行但 HTTP 探测失败，请稍后重试或检查端口"
fi

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
echo
info "部署完成！服务已随系统自启。${NAME:+（租户实例: $NAME）}"
if [ "$UPGRADING" -eq 1 ]; then
    echo "  本次为升级，原有配置沿用；回滚：用 $INSTALL_DIR/goweb.bak 覆盖后重启服务"
else
    echo "  首次安装：请访问 http://${IP:-<服务器IP>}:$PORT/console 完成初始化配置"
    echo "  （配置对象存储、SMTP 邮件服务，并设置管理员邮箱）"
fi
echo "  常用命令：systemctl status $SERVICE_NAME | journalctl -u $SERVICE_NAME -f"
