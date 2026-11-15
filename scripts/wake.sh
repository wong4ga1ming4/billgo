#!/bin/sh
# 被唤醒方 host 上的强制命令脚本。
#
# authorized_keys 里每个"谁能叫醒谁"一行，容器名写死在命令行里：
#   restrict,command="/opt/billgo/wake.sh peach" ssh-ed25519 AAAA... apple
#
# 这样发起方无法指定别的容器，脚本也不需要从 stdin 读任何参数。
set -eu

container="${1:-}"
if [ -z "$container" ]; then
  echo '{"ok":false,"error":"缺少容器名"}'
  exit 2
fi

# 容器名只允许常见字符，防止被当作别的参数注入。
case "$container" in
  *[!A-Za-z0-9_.-]*)
    echo '{"ok":false,"error":"非法容器名"}'
    exit 2
    ;;
esac

if ! docker start "$container" >/dev/null 2>&1; then
  echo '{"ok":false,"error":"docker start 失败"}'
  exit 1
fi
echo '{"ok":true}'
