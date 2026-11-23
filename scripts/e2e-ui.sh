#!/usr/bin/env bash
# billui 冒烟：未登录进不去，密码登录后能看到本节点。
set -euo pipefail

cd "$(dirname "$0")/.."
WORK=$(mktemp -d /tmp/bg-ui.XXXXXX)
PIDS=""
cleanup() {
  if [ -n "${PIDS:-}" ]; then
    # shellcheck disable=SC2086
    kill $PIDS 2>/dev/null || true
    # shellcheck disable=SC2086
    wait $PIDS 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> 编译"
go build -o "$WORK/billgod" ./cmd/billgod
go build -o "$WORK/billgo" ./cmd/billgo
go build -o "$WORK/billui" ./cmd/billui
NET_PORT=18991
UI_PORT=18992
BASE="http://127.0.0.1:$UI_PORT"

mkdir -p "$WORK/data"
cat >"$WORK/config.json" <<EOF
{
  "name": "apple",
  "kind": "resident",
  "network": "ui-e2e",
  "listen": "127.0.0.1:$NET_PORT",
  "data_dir": "$WORK/data",
  "anti_entropy_sec": 1,
  "accept_timeout_min": 5,
  "deliver_timeout_min": 5,
  "job_timeout_min": 1
}
EOF

echo "==> 启动节点和页面"
"$WORK/billgod" -config "$WORK/config.json" >"$WORK/node.log" 2>&1 &
PIDS="$!"
"$WORK/billui" -data "$WORK/data" -listen "127.0.0.1:$UI_PORT" >"$WORK/ui.log" 2>&1 &
PIDS="$PIDS $!"
for _ in $(seq 1 100); do
  "$WORK/billgo" --data "$WORK/data" node >/dev/null 2>&1 && break
  sleep 0.1
done
"$WORK/billgo" --data "$WORK/data" node >/dev/null
for _ in $(seq 1 50); do
  curl -sf -o /dev/null "$BASE/login" && break
  sleep 0.1
done

contains() {
  case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac
}

echo "==> 未登录不能看主页"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/")
[ "$code" = "303" ] || { echo "未登录应 303，得到 $code" >&2; exit 1; }

pw=$(tr -d '[:space:]' <"$WORK/data/billui.password")
[ -n "$pw" ] || { echo "没有密码文件" >&2; cat "$WORK/ui.log" >&2; exit 1; }
contains "$(cat "$WORK/ui.log")" "$pw" || { echo "日志里没有密码" >&2; exit 1; }

echo "==> 登录后看到本节点"
curl -fsS -c "$WORK/cj" -o /dev/null -d "password=$pw" "$BASE/login"
page=$(curl -fsS -b "$WORK/cj" "$BASE/")
contains "$page" "apple" || { echo "主页没有节点名: $page" >&2; exit 1; }
contains "$page" "远程读默认开放" || { echo "主页没有读开放提示: $page" >&2; exit 1; }
contains "$(cat "$WORK/node.log")" "远程读默认开放" || { echo "节点日志没有读开放警告" >&2; cat "$WORK/node.log" >&2; exit 1; }
audit=$(curl -fsS -b "$WORK/cj" "$BASE/audit")
contains "$audit" "没有记录" || { echo "审计页不对: $audit" >&2; exit 1; }
contains "$page" "default-src" && { echo "页面正文不应带 CSP 文本" >&2; exit 1; }
hdr=$(curl -fsS -D - -o /dev/null -b "$WORK/cj" "$BASE/")
contains "$hdr" "default-src 'none'" || { echo "CSP 不对: $hdr" >&2; exit 1; }
contains "$hdr" "no-store" || { echo "缺少 no-store" >&2; exit 1; }

echo "ui e2e 通过"
