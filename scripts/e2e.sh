#!/usr/bin/env bash
# 用真实二进制跑一遍闭环：三个节点、两个 CLI、一张两跳工单。
# 不依赖 Docker，只依赖本机能编译、能跑 ssh 客户端。
set -euo pipefail

cd "$(dirname "$0")/.."
WORK=$(mktemp -d /tmp/bg-e2e.XXXXXX)
PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> 编译"
go build -o "$WORK/billgod" ./cmd/billgod
go build -o "$WORK/billgo" ./cmd/billgo
DAEMON="$WORK/billgod"
CLI="$WORK/billgo"

wait_ready() {
  local data=$1
  for _ in $(seq 1 100); do
    if "$CLI" --data "$data" node >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "节点没有就绪: $data" >&2
  return 1
}

peer_id_of() {
  "$CLI" --data "$1" node | awk '/^peer_id/{print $2}'
}

start_node() {
  local name=$1
  "$DAEMON" -config "$WORK/$name/config.json" >"$WORK/$name/log.txt" 2>&1 &
  LAST_PID=$!
  PIDS+=("$LAST_PID")
  wait_ready "$WORK/$name/data"
}

# write_cfg <name> <port> <allow_json> <bootstrap_json> <agent_json>
write_cfg() {
  local name=$1 port=$2 allow=$3 boot=$4 agent=$5
  mkdir -p "$WORK/$name"
  cat >"$WORK/$name/config.json" <<EOF
{
  "name": "$name",
  "desc": "e2e $name",
  "kind": "resident",
  "network": "e2e",
  "listen": "127.0.0.1:$port",
  "data_dir": "$WORK/$name/data",
  "allow_from": $allow,
  "bootstrap": $boot,
  "agent_cmd": $agent,
  "anti_entropy_sec": 1,
  "accept_timeout_min": 5,
  "deliver_timeout_min": 5,
  "job_timeout_min": 1
}
EOF
}

echo "==> 起 banana，拿到 peer_id（bootstrap 必须带指纹）"
write_cfg banana 18792 '[]' '[]' '[]'
start_node banana
BANANA_PID=$LAST_PID
BANANA_ID=$(peer_id_of "$WORK/banana/data")
echo "    banana=$BANANA_ID"

echo "==> 起 apple，bootstrap 指向 banana"
write_cfg apple 18791 '[]' "[\"https://127.0.0.1:18792#$BANANA_ID\"]" '[]'
start_node apple
APPLE_ID=$(peer_id_of "$WORK/apple/data")
echo "    apple=$APPLE_ID"

echo "==> 带白名单重启 banana，再起 peach"
kill "$BANANA_PID"
wait "$BANANA_PID" 2>/dev/null || true
sleep 0.2
write_cfg banana 18792 "[\"$APPLE_ID\"]" "[\"https://127.0.0.1:18791#$APPLE_ID\"]" \
  '["/bin/sh","-c","echo banana-done > \"$BILLGO_OUT_DIR/result.txt\""]'
start_node banana

write_cfg peach 18793 "[\"$APPLE_ID\"]" "[\"https://127.0.0.1:18792#$BANANA_ID\"]" \
  '["/bin/sh","-c","echo peach-done > \"$BILLGO_OUT_DIR/result.txt\""]'
start_node peach
PEACH_ID=$(peer_id_of "$WORK/peach/data")
echo "    peach=$PEACH_ID"

# 注意：不能用 `cli | grep -q`，pipefail 下 grep 提前退出会让 CLI 吃到 SIGPIPE。
contains() {
  case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac
}

echo "==> 等名录收敛"
for _ in $(seq 1 100); do
  peers=$("$CLI" --data "$WORK/apple/data" peers 2>/dev/null || true)
  if contains "$peers" "$PEACH_ID"; then
    break
  fi
  sleep 0.1
done
"$CLI" --data "$WORK/apple/data" peers

echo "==> 发起两跳工单"
cat >"$WORK/form.json" <<'EOF'
{"task":"写一份 e2e 报告","format":"markdown"}
EOF
TID=$("$CLI" --data "$WORK/apple/data" ticket create --form "$WORK/form.json" --flow banana,peach)
echo "    ticket=$TID"

is_closed() {
  local out
  out=$("$CLI" --data "$WORK/apple/data" ticket show "$TID" 2>/dev/null) || return 1
  contains "$out" "[closed]"
}

for _ in $(seq 1 300); do
  if is_closed; then
    break
  fi
  sleep 0.1
done

echo "==> 结果"
"$CLI" --data "$WORK/apple/data" ticket show "$TID"
if ! is_closed; then
  echo "工单没有完成" >&2
  for n in apple banana peach; do
    echo "--- $n log ---" >&2
    cat "$WORK/$n/log.txt" >&2 || true
  done
  exit 1
fi

echo "==> 校验回执与交付物"
blobs=$(ls -1 "$WORK/apple/data/blobs" | wc -l | tr -d ' ')
if [ "$blobs" -lt 2 ]; then
  echo "apple 上应有两个交付物，实际 $blobs" >&2
  exit 1
fi
echo "e2e 通过"
