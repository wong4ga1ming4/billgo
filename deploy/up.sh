#!/usr/bin/env bash
# 一键起三节点 demo：banana / apple / peach，跑完一张两跳工单。
#
# 为什么分两步：bootstrap 必须带对方的 peer_id（TLS 指纹固定），
# 而 peer_id 要等对方第一次启动才生成。所以先起节点、读 id、再写配置。
set -euo pipefail

cd "$(dirname "$0")"
CONF=./conf
mkdir -p "$CONF"

dc() { docker compose "$@"; }

# 容器内执行 CLI
cli() {
  local svc=$1
  shift
  dc exec -T "$svc" billgo --data /data "$@"
}

wait_ready() {
  local svc=$1
  for _ in $(seq 1 120); do
    if cli "$svc" node >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "$svc 没就绪" >&2
  dc logs "$svc" >&2 || true
  return 1
}

peer_id_of() {
  cli "$1" node | awk '/^peer_id/{print $2}'
}

cat >"$CONF/banana.json" <<'EOF'
{
  "name": "banana",
  "desc": "demo 第一站",
  "kind": "resident",
  "network": "demo",
  "listen": "0.0.0.0:8787",
  "endpoint": "https://banana:8787",
  "data_dir": "/data",
  "allow_from": [],
  "bootstrap": [],
  "agent_cmd": ["/bin/sh", "-c", "echo banana-done > \"$BILLGO_OUT_DIR/result.txt\""],
  "anti_entropy_sec": 2,
  "job_timeout_min": 1
}
EOF

cat >"$CONF/apple.json" <<'EOF'
{
  "name": "apple",
  "desc": "demo 发起者",
  "kind": "resident",
  "network": "demo",
  "listen": "0.0.0.0:8787",
  "endpoint": "https://apple:8787",
  "data_dir": "/data",
  "allow_from": [],
  "bootstrap": [],
  "anti_entropy_sec": 2,
  "job_timeout_min": 1
}
EOF

echo "==> 构建并启动 banana（先空跑一次拿 peer_id）"
dc build
dc up -d banana
wait_ready banana
BANANA_ID=$(peer_id_of banana)
echo "    banana=$BANANA_ID"

echo "==> 写 apple 配置并启动"
python3 - "$CONF/apple.json" "$BANANA_ID" <<'PY'
import json, sys
p, bid = sys.argv[1], sys.argv[2]
c = json.load(open(p))
c["bootstrap"] = [f"https://banana:8787#{bid}"]
json.dump(c, open(p, "w"), ensure_ascii=False, indent=2)
PY
dc up -d apple
wait_ready apple
APPLE_ID=$(peer_id_of apple)
echo "    apple=$APPLE_ID"

echo "==> 带白名单重启 banana；写 peach 配置并启动"
python3 - "$CONF/banana.json" "$APPLE_ID" <<'PY'
import json, sys
p, aid = sys.argv[1], sys.argv[2]
c = json.load(open(p))
c["allow_from"] = [aid]
c["bootstrap"] = [f"https://apple:8787#{aid}"]
json.dump(c, open(p, "w"), ensure_ascii=False, indent=2)
PY
dc restart banana >/dev/null
wait_ready banana

cat >"$CONF/peach.json" <<EOF
{
  "name": "peach",
  "desc": "demo 第二站",
  "kind": "resident",
  "network": "demo",
  "listen": "0.0.0.0:8787",
  "endpoint": "https://peach:8787",
  "data_dir": "/data",
  "allow_from": ["$APPLE_ID"],
  "bootstrap": ["https://banana:8787#$BANANA_ID"],
  "agent_cmd": ["/bin/sh", "-c", "echo peach-done > \"\$BILLGO_OUT_DIR/result.txt\""],
  "anti_entropy_sec": 2,
  "job_timeout_min": 1
}
EOF
dc up -d peach
wait_ready peach
PEACH_ID=$(peer_id_of peach)
echo "    peach=$PEACH_ID"

echo "==> 等 apple 看见全部节点"
for _ in $(seq 1 120); do
  out=$(cli apple peers 2>/dev/null || true)
  case "$out" in *"$PEACH_ID"*) break ;; esac
  sleep 0.5
done
cli apple peers

echo "==> 发起工单"
dc exec -T apple sh -c 'printf "{\"task\":\"demo 报告\"}" > /tmp/form.json'
TID=$(dc exec -T apple billgo --data /data ticket create --form /tmp/form.json --flow banana,peach)
echo "    ticket=$TID"

echo "==> 等待完成"
for _ in $(seq 1 240); do
  out=$(cli apple ticket show "$TID" 2>/dev/null || true)
  case "$out" in *"[closed]"*) break ;; esac
  sleep 0.5
done
cli apple ticket show "$TID"

out=$(cli apple ticket show "$TID")
case "$out" in
  *"[closed]"*) echo "demo 通过" ;;
  *) echo "demo 未完成" >&2; exit 1 ;;
esac
