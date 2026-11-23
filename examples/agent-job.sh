#!/bin/sh
# billgo Agent 适配脚本示例。
#
# 契约（billgo 只知道这五条，不知道你用什么 Agent）：
#   输入   $BILLGO_JOB_DIR/job.json + BILLGO_* 环境变量
#   产物   写到 $BILLGO_OUT_DIR
#   拒绝   写 $BILLGO_JOB_DIR/result.json 的 reason，并非零退出
#   成功   退出码 0，billgo 会把 $BILLGO_OUT_DIR 打包成交付物
#   超时   billgo 按 job_timeout_min 杀进程并记为拒绝
#
# 换 Agent 只改第 2 步；billgo 核心不用动。
# 需要 python3 解析 JSON；没有的话换成 jq 或 node。
set -eu

: "${BILLGO_JOB_DIR:?缺少 BILLGO_JOB_DIR}"
: "${BILLGO_OUT_DIR:?缺少 BILLGO_OUT_DIR}"

JOB=$BILLGO_JOB_DIR
OUT=$BILLGO_OUT_DIR
mkdir -p "$OUT"

# 1. 把表单变成 prompt。这里假设表单形如：
#    {"task": "...", "materials": [{"name": "...", "content": "..."}]}
python3 - "$JOB/job.json" "$JOB/prompt.md" <<'PY'
import json
import sys

job = json.load(open(sys.argv[1], encoding="utf-8"))
form = job.get("form") or {}
task = form.get("task") or "完成表单里的任务"

text = "# 任务\n\n" + str(task) + "\n\n# 表单\n\n```json\n"
text += json.dumps(form, ensure_ascii=False, indent=2) + "\n```\n"
open(sys.argv[2], "w", encoding="utf-8").write(text)
PY

# 2. 调用 Agent。下面是 pi 的例子：
#
#    pi --model "$PI_MODEL" -p "$(cat "$JOB/prompt.md")"
#
# Claude Code 的例子：
#
#    claude -p "$(cat "$JOB/prompt.md")"
#
# 如果 Agent 要求固定工作目录（例如 /workspace），在调用前 cd 过去；
# 产物仍然要写到 $OUT。
if ! pi --model "${PI_MODEL:-deepseek/deepseek-flash}" \
        -p "$(cat "$JOB/prompt.md")"; then
  printf '{"reason":"Agent 执行失败"}\n' > "$JOB/result.json"
  exit 1
fi

# 3. 没有产物 = 没有交付。不写 result.json 的话 billgo 只会记成执行失败。
if [ -z "$(ls -A "$OUT")" ]; then
  printf '{"reason":"Agent 没有产生交付物"}\n' > "$JOB/result.json"
  exit 1
fi

# 4. 退出 0，billgo 负责打包和送达。
