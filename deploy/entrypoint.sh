#!/bin/sh
# 先起 billgod，再按需起 billui。BILLUI=0 时只有节点。
# billui 使用和节点相同的数据目录，默认 /data，可用 BILLGO_DATA 改。
set -u

billgod "$@" &
core=$!

stop() {
  if [ -n "${ui:-}" ]; then
    kill "$ui" 2>/dev/null || true
  fi
  kill "$core" 2>/dev/null || true
  wait "$core" 2>/dev/null || true
}
trap stop INT TERM

if [ "${BILLUI:-1}" != "0" ]; then
  billui -data "${BILLGO_DATA:-/data}" -listen "${BILLUI_LISTEN:-127.0.0.1:7878}" &
  ui=$!
fi

wait "$core"
status=$?
if [ -n "${ui:-}" ]; then
  kill "$ui" 2>/dev/null || true
fi
exit "$status"
