#!/usr/bin/env bash
# 唤醒端到端：三个节点都在 Docker 网络里，peach 处于停止状态；
# apple 挂载了 docker socket，用 local 方式执行 docker start 把它叫醒，
# 然后派单、交付、闭环。
#
# 需要本机 docker 可用。
set -euo pipefail

cd "$(dirname "$0")/.."
command -v docker >/dev/null || { echo "需要 docker，跳过" >&2; exit 0; }
[ -S /var/run/docker.sock ] || { echo "需要 /var/run/docker.sock，跳过" >&2; exit 0; }

WORK=$(mktemp -d /tmp/bg-wake.XXXXXX)
IMAGE=billgo:dev
SUFFIX=$RANDOM
NET=bg-wake-net-$SUFFIX
BANANA=bg-wake-banana-$SUFFIX
APPLE=bg-wake-apple-$SUFFIX
PEACH=bg-wake-peach-$SUFFIX

cleanup() {
  for c in "$PEACH" "$APPLE" "$BANANA"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> 构建镜像"
docker build -q -t "$IMAGE" . >/dev/null
docker network create "$NET" >/dev/null

# run_node <容器名> <网络别名> <配置文件名> [额外 docker run 参数...]
run_node() {
  local name=$1 alias=$2 cfg=$3
  shift 3
  mkdir -p "$WORK/$name-data"
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --network "$NET" --network-alias "$alias" \
    -v "$WORK/$cfg:/etc/billgo/config.json:ro" \
    -v "$WORK/$name-data:/data" "$@" "$IMAGE" >/dev/null
}

# cli <容器名> <参数...>
cli() {
  local name=$1
  shift
  docker exec "$name" billgo --data /data "$@"
}

wait_ready() {
  local name=$1
  for _ in $(seq 1 150); do
    cli "$name" node >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "$name 没就绪" >&2
  docker logs "$name" >&2 || true
  return 1
}

peer_id_of() { cli "$1" node | awk '/^peer_id/{print $2}'; }

write_cfg() {
  local file=$1 name=$2 allow=$3 boot=$4 kind=$5 agent=$6 extra=$7
  cat >"$WORK/$file" <<EOF
{
  "name": "$name",
  "desc": "wake e2e $name",
  "kind": "$kind",
  "network": "wake-e2e",
  "listen": "0.0.0.0:8787",
  "endpoint": "https://$name:8787",
  "data_dir": "/data",
  "allow_from": $allow,
  "bootstrap": $boot,
  "agent_cmd": $agent,
  $extra
  "anti_entropy_sec": 1,
  "accept_timeout_min": 5,
  "deliver_timeout_min": 5,
  "job_timeout_min": 1
}
EOF
}

echo "==> 起 banana，拿 peer_id"
write_cfg banana.json banana '[]' '[]' resident '[]' ''
run_node "$BANANA" banana banana.json
wait_ready "$BANANA"
BANANA_ID=$(peer_id_of "$BANANA")
echo "    banana=$BANANA_ID"

echo "==> 起 apple（挂 docker socket），bootstrap 指向 banana"
write_cfg apple.json apple '[]' "[\"https://banana:8787#$BANANA_ID\"]" resident '[]' ''
run_node "$APPLE" apple apple.json -v /var/run/docker.sock:/var/run/docker.sock
wait_ready "$APPLE"
APPLE_ID=$(peer_id_of "$APPLE")
echo "    apple=$APPLE_ID"

echo "==> 带白名单重启 banana"
write_cfg banana.json banana "[\"$APPLE_ID\"]" "[\"https://apple:8787#$APPLE_ID\"]" resident '[]' ''
docker restart "$BANANA" >/dev/null
wait_ready "$BANANA"

echo "==> 起 peach（唤醒类，local 唤醒），再停掉"
write_cfg peach.json peach "[\"$APPLE_ID\"]" "[\"https://banana:8787#$BANANA_ID\"]" wakeable \
  '["/bin/sh", "-c", "echo peach-woken > \"$BILLGO_OUT_DIR/result.txt\""]' \
  "\"wake\": {\"mode\": \"local\", \"container\": \"$PEACH\"},"
run_node "$PEACH" peach peach.json
wait_ready "$PEACH"

echo "==> 等 apple 的名录里出现 peach"
found=0
for _ in $(seq 1 150); do
  out=$(cli "$APPLE" peers 2>/dev/null || true)
  case "$out" in *"peach#"*) found=1; break ;; esac
  sleep 0.2
done
cli "$APPLE" peers
if [ "$found" != 1 ]; then
  echo "peach 没有登记" >&2
  docker logs "$PEACH" >&2 || true
  exit 1
fi

echo "==> 停掉 peach（唤醒类不应产生墓碑）"
docker stop "$PEACH" >/dev/null
sleep 0.5
out=$(cli "$APPLE" peers)
case "$out" in
  *"peach#"*) ;;
  *) echo "peach 停掉后从名录消失了" >&2; exit 1 ;;
esac
case "$out" in
  *"peach#"*"[已注销]"*) echo "唤醒类不应自动注销" >&2; exit 1 ;;
esac
echo "    peach 仍在名录里"

echo "==> apple 发单，触发 local 唤醒"
docker exec "$APPLE" sh -c 'printf "{\"task\":\"叫醒 peach\"}" > /tmp/form.json'
TID=$(cli "$APPLE" ticket create --form /tmp/form.json --flow peach)
echo "    ticket=$TID"

for _ in $(seq 1 300); do
  out=$(cli "$APPLE" ticket show "$TID" 2>/dev/null || true)
  case "$out" in *"[closed]"*) break ;; esac
  sleep 0.2
done
cli "$APPLE" ticket show "$TID"

out=$(cli "$APPLE" ticket show "$TID")
case "$out" in
  *"[closed]"*)
    running=$(docker inspect -f '{{.State.Running}}' "$PEACH" 2>/dev/null || echo false)
    if [ "$running" != "true" ]; then
      echo "工单完成了，但 peach 容器没有被拉起来，唤醒路径没被验证到" >&2
      exit 1
    fi
    echo "wake e2e 通过（peach 确实由 apple 唤醒）"
    ;;
  *)
    echo "唤醒后工单未完成" >&2
    docker logs "$APPLE" >&2 || true
    docker logs "$BANANA" >&2 || true
    docker logs "$PEACH" >&2 || true
    exit 1
    ;;
esac
