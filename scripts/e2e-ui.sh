#!/usr/bin/env bash
# 本机页面冒烟：内置页、接口、页面发现、发起工单、CSP。
set -euo pipefail

cd "$(dirname "$0")/.."
WORK=$(mktemp -d /tmp/bg-ui.XXXXXX)
PID=""
cleanup() {
  if [ -n "${PID:-}" ]; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> 编译"
go build -o "$WORK/billgod" ./cmd/billgod
go build -o "$WORK/billgo" ./cmd/billgo
DAEMON="$WORK/billgod"
CLI="$WORK/billgo"
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
  "ui": {"listen": "127.0.0.1:$UI_PORT"},
  "anti_entropy_sec": 1,
  "accept_timeout_min": 5,
  "deliver_timeout_min": 5,
  "job_timeout_min": 1
}
EOF

echo "==> 启动节点"
"$DAEMON" -config "$WORK/config.json" >"$WORK/log.txt" 2>&1 &
PID=$!
for _ in $(seq 1 100); do
  "$CLI" --data "$WORK/data" node >/dev/null 2>&1 && break
  sleep 0.1
done
if ! "$CLI" --data "$WORK/data" node >/dev/null 2>&1; then
  cat "$WORK/log.txt" >&2
  exit 1
fi

contains() {
  case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac
}

echo "==> 内置页面与接口"
page=$(curl -fsS "$BASE/")
contains "$page" "billgo" || { echo "主页内容不对" >&2; exit 1; }

node_json=$(curl -fsS "$BASE/api/node")
contains "$node_json" '"peer_id"' || { echo "缺少 peer_id" >&2; exit 1; }
contains "$node_json" '"version"' || { echo "缺少 version" >&2; exit 1; }
PEER_ID=$(printf '%s' "$node_json" | sed -n 's/.*"peer_id":"\([^"]*\)".*/\1/p')
[ -n "$PEER_ID" ] || { echo "解析 peer_id 失败" >&2; exit 1; }

peers=$(curl -fsS "$BASE/api/peers")
if contains "$peers" "pubkey"; then
  echo "名录接口泄露了公钥" >&2
  exit 1
fi

echo "==> 页面发现"
printf '<html><title>Hello from Agent</title></html>' >"$WORK/data/www/pages/hello.html"
pages=$(curl -fsS "$BASE/api/pages")
contains "$pages" "Hello from Agent" || { echo "新增页面没有被发现: $pages" >&2; exit 1; }
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/pages/hello.html")
[ "$code" = "200" ] || { echo "页面访问返回 $code" >&2; exit 1; }

echo "==> 发起工单"
form='{"task":"ui smoke"}'
resp=$(curl -fsS -X POST "$BASE/api/tickets" \
  -H 'Content-Type: application/json' \
  --data "{\"form\":$form,\"flow\":[\"$PEER_ID\"]}")
contains "$resp" '"id"' || { echo "创建工单失败: $resp" >&2; exit 1; }
TID=$(printf '%s' "$resp" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$TID" ] || { echo "解析工单号失败" >&2; exit 1; }

list=$(curl -fsS "$BASE/api/tickets")
contains "$list" "$TID" || { echo "工单没有出现在列表里" >&2; exit 1; }
detail=$(curl -fsS "$BASE/api/tickets/$TID")
contains "$detail" "$TID" || { echo "工单详情不对" >&2; exit 1; }

echo "==> 安全头"
hdr=$(curl -s -D - -o /dev/null "$BASE/")
contains "$hdr" "connect-src 'self'" || { echo "内置页 CSP 不对" >&2; exit 1; }
contains "$hdr" "no-store" || { echo "缺少 no-store" >&2; exit 1; }
hdr=$(curl -s -D - -o /dev/null "$BASE/pages/hello.html")
contains "$hdr" "'unsafe-inline'" || { echo "Agent 页 CSP 不对" >&2; exit 1; }

echo "ui e2e 通过"
