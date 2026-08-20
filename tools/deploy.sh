#!/usr/bin/env bash
# 构建并部署 Windows 启动器。
# 后台服务使用数据目录中的服务映像；部署时只停止面板进程，不停止后台代理。
# 启动器部署完成后，打开面板并执行「保存并应用」以更新服务映像。

set -euo pipefail

cd "$(dirname "$0")/.."
export PATH="$PATH:$HOME/.local/go/bin"
PS=/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe
# 未设置 CCPROXY_DEST 时部署到 Windows 桌面，并校验目标目录存在。
if [ -z "${CCPROXY_DEST:-}" ]; then
  profile=$("$PS" -NoProfile -Command '$env:USERPROFILE' 2>/dev/null | tr -d '\r')
  [ -n "$profile" ] || { echo "取不到 %USERPROFILE%，请用 CCPROXY_DEST 指定目标路径"; exit 1; }
  DST="$(wslpath "$profile")/Desktop/ccproxy.exe"
else
  DST=$CCPROXY_DEST
fi
[ -d "$(dirname "$DST")" ] || { echo "目标目录不存在: $(dirname "$DST")"; exit 1; }

say() { printf '%s\n' "$*"; }

# ---------- 构建前的门槛 ----------
say "· 校验"
test -z "$(gofmt -l .)" || { gofmt -l . ; echo "gofmt 未通过"; exit 1; }
go vet ./... >/dev/null
go test ./... >/dev/null
node --check ui/app.js
python3 tools/htmlcheck.py ui/index.html

say "· 构建"
# -trimpath 移除构建机源码路径，避免将本地路径写入二进制。
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-H windowsgui -s -w" -o /tmp/ccproxy-rel.exe .

# ---------- 清场：只杀面板 ----------
killed=$("$PS" -NoProfile -Command "
  \$n = 0
  Get-CimInstance Win32_Process -Filter \"Name='ccproxy.exe'\" |
    Where-Object { \$_.CommandLine -notmatch '--daemon' } |
    ForEach-Object { Stop-Process -Id \$_.ProcessId -Force -EA SilentlyContinue; \$n++ }
  \$n" 2>/dev/null | tr -d '\r ')
sleep 0.4

# ---------- 落盘 ----------
cp /tmp/ccproxy-rel.exe "$DST"

# ---------- 核对 ----------
win=$(printf '%s' "$DST" | sed 's|^/mnt/\([a-z]\)/|\U\1:/|')
got=$("$PS" -NoProfile -Command "(Get-FileHash '$win' -Algorithm SHA256).Hash" 2>/dev/null | tr -d '\r')
want=$(sha256sum /tmp/ccproxy-rel.exe | cut -d' ' -f1 | tr 'a-f' 'A-F')
[ "$got" = "$want" ] || { say "✗ 哈希不一致：$got ≠ $want"; exit 1; }
say "· 已部署 ${DST}  ${want:0:16}…"
