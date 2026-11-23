# billgo

去中心化的 Agent 之间派单工具。一个二进制，既是客户端也是服务端；每个节点本地持有完整名录；工单由发起者持有，接活方只留一张签名回执。

v1 的目标很窄：**内网、线性工单流转、能跑起来**。

- 设计：[docs/全流程设计.md](docs/全流程设计.md)
- 计划与验收：[docs/v1开发计划.md](docs/v1开发计划.md)

## 安装

macOS / Linux：

```bash
curl -fsSL https://raw.githubusercontent.com/wong4ga1ming4/billgo/main/scripts/install.sh | sh
```

Windows（PowerShell）：

```powershell
irm https://raw.githubusercontent.com/wong4ga1ming4/billgo/main/scripts/install.ps1 | iex
```

两个脚本都会下载对应平台的发行包、校验 SHA256，然后安装 `billgo`、`billgod` 与 `billui`。也可以直接从 [Releases](https://github.com/wong4ga1ming4/billgo/releases) 下载：

| 平台 | 包 |
|---|---|
| macOS Intel / Apple Silicon | `billgo_<version>_darwin_amd64.tar.gz` / `_arm64.tar.gz` |
| Linux x86_64 / arm64 | `billgo_<version>_linux_amd64.tar.gz` / `_arm64.tar.gz` |
| Windows x64 / arm64 | `billgo_<version>_windows_amd64.zip` / `_arm64.zip` |

指定版本或目录：

```bash
VERSION=v1.0.0 INSTALL_DIR="$HOME/bin" sh install.sh
```

```powershell
.\install.ps1 -Version v1.0.0 -InstallDir C:\tools\billgo
```

网络访问 GitHub 受限时，可以把 `BASE_URL`（Windows 是 `-BaseUrl`）指向镜像或本地目录：

```bash
VERSION=v1.0.0 BASE_URL=https://mirror.example.com/billgo sh install.sh
```

## 本机页面

页面是单独的 `billui` 进程，不在 `billgod` 里。它只问本机节点。默认监听 `127.0.0.1:7878`：

```bash
billui -data data
```

浏览器打开 `http://127.0.0.1:7878/`。每次启动都会生成新密码，打在日志里，并写入 `data/billui.password`（权限 0600）。重启后旧会话失效。

可以看本节点的状态、配置摘要、名录和工单，也可以点开一个邻居看它此刻的配置和工单。工单列表不含表单。配置或内容被拒时，列表还在，打开单张工单会显示「内容不可见」。离线显示「离线/不可达」，超时显示「超时」。都不自动重试，点「再试一次」才会再问，不保留上次的内容。读默认开放时首页会提示。审计页只看谁访问过本机。页面上可以发起、改派、作废。改配置用 `billgo config write`。

容器里默认同样只绑回环。要从宿主机打开：

```bash
docker run -e BILLUI_LISTEN=0.0.0.0:7878 -p 127.0.0.1:7878:7878 ...
```

不要把端口公布到 `0.0.0.0`。`BILLUI=0` 时容器只跑节点。监听地址可以不是 loopback，因为页面有密码。

## 一分钟跑通

不需要 Docker，一台机器即可：

```bash
./scripts/e2e.sh
```

脚本会编译出 `billgod` 和 `billgo`，起三个节点（banana / apple / peach），发起一张两跳工单，等它闭环，并校验回执与交付物。

用 Docker 起三节点 demo：

```bash
./deploy/up.sh          # 构建、起容器、跑一张工单
docker compose -f deploy/compose.yaml down -v   # 清理
```

## 它解决什么问题

一个 Agent（容器里的 pi / Claude Code / 任何程序）需要把任务交给另一个 Agent，并拿回交付物。这件事不应该依赖一个中心服务：

- **身份自证**：`peer_id = base32(sha256(公钥))[:16]`，同一把 ed25519 密钥用于签名、TLS 和 SSH。
- **没有注册中心**：每个节点本地有一份完整名录，记录由主人自签名，靠广播和反熵保持一致。
- **工单归发起者**：表单、流转图、每一跳的交付物都在发起者本地；接活方只做这一跳，留一张回执。
- **先落盘再发送**：出站信封先进队列，至少一次投递 + 幂等执行。

## 三个二进制

| 命令 | 作用 |
|---|---|
| `billgod` | 节点守护进程：监听 TLS、维护名录、驱动工单、本地控制 socket。没有页面 |
| `billgo` | 命令行：通过数据目录下的 Unix socket 与本机节点对话 |
| `billui` | 可选页面：登录后通过同一个 socket 看本机和邻居。billgod 不依赖它 |

网络端口只认签名信封；控制面只对本机开放（socket 权限 0600）。

## 配置

`billgod -config billgo.json`。所有字段：

```json
{
  "name": "peach",
  "desc": "写文档的节点",
  "kind": "resident",
  "network": "default",
  "listen": "0.0.0.0:8787",
  "endpoint": "https://10.0.0.3:8787",
  "data_dir": "/data",
  "bootstrap": ["https://10.0.0.1:8787#wln2y4zmi2pcjeni"],
  "allow_from": ["wln2y4zmi2pcjeni"],
  "wake": {
    "mode": "ssh",
    "target": "user@10.0.0.3",
    "container": "peach",
    "host_fp": "SHA256:..."
  },
  "agent_cmd": ["/bin/sh", "-c", "echo done > \"$BILLGO_OUT_DIR/result.txt\""],
  "accept_timeout_min": 30,
  "deliver_timeout_min": 120,
  "job_timeout_min": 60,
  "anti_entropy_sec": 60
}
```

要点：

- `bootstrap` 必须写成 `https://host:port#peer_id`。第一次连接就要能固定 TLS 指纹，没有它就没有身份。
- `allow_from` 是派单白名单，**空表示全部拒绝**。填发起者的 `peer_id`。
- `endpoint` 是别人连你用的地址。监听 `0.0.0.0` 或跑在容器里时必须显式填写。
- `wake` 只有 `kind: "wakeable"` 必填。`mode` 为 `ssh`（需 `target`）或 `local`（只需 `container`）；`agent_cmd` 是接活时执行的命令，为空表示不接受作业。
- 节点配置里没有页面字段。页面用 `billui` 的 `-listen` 或 `BILLUI_LISTEN`。
- 换 `network` 就是另一个域，记录互相隔离。

### 环境变量

容器部署时可以用环境变量覆盖配置，优先级 **环境变量 > 配置文件 > 默认值**。

- 只有变量存在才覆盖；没设置就沿用文件。
- 设置成空字符串也算覆盖。
- 合并后仍会走校验，填错直接启动失败。

| 变量 | 字段 | 格式 | 空值 / 错误时 |
|---|---|---|---|
| `BILLGO_NAME` | `name` | 任意字符串 | 空 → 启动失败 |
| `BILLGO_KIND` | `kind` | `resident` / `wakeable` | 其他值 → 启动失败 |
| `BILLGO_NETWORK` | `network` | 域标识 | 空 → 启动失败 |
| `BILLGO_ENDPOINT` | `endpoint` | `https://host:port` | 空 → 按 `listen` 推导（`0.0.0.0` 会变成 `127.0.0.1`） |
| `BILLGO_BOOTSTRAP` | `bootstrap` | 逗号分隔的 `https://host:port#peer_id` | 空 → 不配邻居；格式错 → 启动失败 |
| `BILLGO_ALLOW_FROM` | `allow_from` | 逗号分隔的 16 位 peer_id | 空 → **全部拒绝** |

几个容易踩的点：

- **`BILLGO_BOOTSTRAP`**：`#` 后面的 16 位 peer_id 不能省，第一次连接就要固定 TLS 指纹；只配一个邻居就够，其余节点靠反熵发现；多个用逗号分隔，元素里不能有逗号。
- **`BILLGO_ALLOW_FROM`**：这是**接收方**的白名单，不是发起方授权；必须写 peer_id，不能写名字；不设置或为空等于谁都拒。
- **`BILLGO_ENDPOINT`**：别人连你用的地址。容器里要写服务名（如 `https://apple:8787`），写 `127.0.0.1` 对方会连到它自己。
- **`BILLGO_KIND`**：改成 `wakeable` 时，`wake` 信息仍然必须写在配置文件里。

故意没放进环境变量的：`listen`、`data_dir`、`agent_cmd`、`wake`、超时和反熵周期，继续写文件。页面监听是 `BILLUI_LISTEN`，不属于节点配置。

配置生效后可以自查：

```bash
billgo --data /data node      # name / kind / network / endpoint
billgo --data /data peers     # 名录是否拉到了
```

容器示例：

```bash
docker run -d --name apple --network billgo-manual \
  -e BILLGO_NAME=apple \
  -e BILLGO_ENDPOINT=https://apple:8787 \
  -e BILLGO_BOOTSTRAP='https://banana:8787#3ipmrxqyj5sfzlsa' \
  -v apple-data:/data \
  billgo:dev
```

## 加入集群

第一个节点不需要邻居，`bootstrap` 留空即可：

```json
{
  "name": "banana",
  "kind": "resident",
  "network": "default",
  "listen": "0.0.0.0:8787",
  "endpoint": "https://banana:8787",
  "data_dir": "/data",
  "bootstrap": [],
  "allow_from": []
}
```

启动后读它的 peer_id：

```bash
billgo --data /data node
billgo --data /data peers    # 只有它自己
```

第二个节点只配第一个节点：

```json
"bootstrap": ["https://banana:8787#<第一个节点的 peer_id>"]
```

第二个节点启动后会拉取名录，并把自己的记录推回第一个节点；所以**第一个节点会自动发现第二个**，不需要在第一个节点里配置 bootstrap。后面每个新节点都只配一个已有节点即可。

如果第一个节点要接第二个节点的派单，还要把第二个节点的 peer_id 加进第一个节点的 `allow_from` 并重启。`bootstrap` 只管发现，不管授权。

两个注意点：

- `endpoint` 必须是别人能连到的地址。容器里写服务名（如 `https://banana:8787`），写 `127.0.0.1` 对方会连到它自己。
- 两边的 `network` 必须一致，否则记录会被静默丢弃。

用环境变量起第一个节点时不需要 `BILLGO_BOOTSTRAP`：

```bash
docker run -d --name banana --network billgo-manual \
  -e BILLGO_NAME=banana \
  -e BILLGO_ENDPOINT=https://banana:8787 \
  -v banana-data:/data \
  billgo:dev
```

完整的三节点手工流程见 [docs/手工测试.md](docs/手工测试.md)。

## CLI

```bash
billgo [--data 数据目录] <命令>

billgo node                                   # 节点信息（peer_id 在这里）
billgo status                                 # 状态、待处理与配置摘要
billgo config                                 # 当前生效配置
billgo config write --file cfg.json           # access 立即生效，其余重启后生效
billgo peers                                  # 本地名录
billgo ticket create --form f.json --flow banana,peach
billgo ticket list
billgo ticket show <id>                       # 状态 + 事件流
billgo ticket redispatch <id> --hop 1 --to banana
billgo ticket void <id>
billgo audit                                  # 谁访问过本机，可加 --limit / --cursor
billgo outbox                                 # 卡住的消息与重试次数
billgo wake peach                             # 手动叫醒
billgo withdraw                               # 注销本节点（写墓碑，其他人不再能派单给它）

billgo --peer peach node                      # 问邻居。还有 config / peers
billgo --peer peach ticket list --limit 20    # 分页，没有“全部加载”
billgo --peer peach ticket show <id>
billgo --peer peach ticket create --form f.json --flow banana
billgo --peer peach config write --file cfg.json
```

`--peer` 的读默认允许，写要对方显式放行。拒绝、离线、超时会是不同的错误。工单仍只在发起者本地，问不到离线节点的旧数据。

注销规则：常驻节点优雅退出时自动注销；**唤醒类不会**——睡着是它的正常状态，自动注销等于让别人再也叫不醒它。要下线一个唤醒类节点，显式执行 `billgo withdraw`。

数据目录默认 `data`，也可用 `BILLGO_DATA` 指定。

## 接活方怎么干活

守护进程收到 `hop.issue` 后：

1. 验签与白名单通过，立刻回 `hop.accept`；
2. 写 `job.json`，启动 `agent_cmd`，并通过环境变量告诉它上下文：

| 变量 | 含义 |
|---|---|
| `BILLGO_TICKET` / `BILLGO_HOP` | 工单号与跳号 |
| `BILLGO_JOB_DIR` | 作业目录（写 `result.json` 可给出拒绝原因） |
| `BILLGO_OUT_DIR` | 交付物目录，进程退出后整体打包 |
| `BILLGO_FROM` | 发起者 peer_id |

3. 退出码 0：`out/` 按内容寻址打包，签名回执，发 `hop.deliver`；
4. 退出码非 0 或超时：把 `result.json` 里的 `reason` 作为拒绝原因发 `hop.reject`（没有就写"执行失败"）。

Agent 一侧不需要知道信封、哈希、签名。一个 shell 脚本和一个 LLM Agent 接活方式完全相同。

作业状态落库，守护进程重启后会重新执行未完成的作业。每个节点同时只跑一个作业。

### 适配脚本

完整示例见 [examples/agent-job.sh](examples/agent-job.sh)。契约只有五条：

| 项 | 约定 |
|---|---|
| 输入 | 读 `$BILLGO_JOB_DIR/job.json` |
| 产物 | 写到 `$BILLGO_OUT_DIR` |
| 拒绝 | 写 `$BILLGO_JOB_DIR/result.json` 的 `reason`，非零退出 |
| 成功 | 退出码 0，billgo 负责打包和送达 |
| 超时 | `job_timeout_min` 到点杀进程，按拒绝处理 |

换 Agent 只改脚本里的调用行（`pi`、`claude`、任意程序），billgo 核心不用动。`agent_cmd` 为空表示这个节点不接活。

## 唤醒

两种方式，写在 peer 记录的 `wake` 字段里，都不进信封协议。

**`mode: "ssh"`（默认）**：凭证由使用方手工配置，被唤醒机器的 `authorized_keys`：

```
restrict,command="/opt/billgo/wake.sh peach" ssh-ed25519 AAAA... apple
```

一行授权"apple 可以叫醒 peach"。脚本只做 `docker start <硬编码容器名>`。权限边界在 sshd：billgod 不需要 docker 权限，只能触发那一条命令。

**`mode: "local"`**：billgod 直接执行 `docker start <container>`，容器名会做字符集校验。适合两种部署：

- billgod 跑在宿主机上（最干净，零凭证、零 sshd 依赖）；
- billgod 在容器里，但挂载了 `/var/run/docker.sock`。

第二种要清楚代价：**挂载 docker socket 等价于把宿主机 root 交给容器**。billgod 自己只会执行一条固定的 `docker start`，但权限边界由宿主机决定，不在工具里。接受这个代价就用 local，不接受就用 ssh。

```json
{"mode": "local", "container": "peach"}
```

不管哪种方式，发起方在派单前都会先查 `/health`，不通才唤醒，叫醒后轮询到就绪再发信封。

## 名录怎么保持一致

- 每个节点存全部记录，每条记录由主人签名。
- 记录内容变化时广播给已知节点（尽力而为）。
- 每 `anti_entropy_sec` 秒随机挑一个节点交换摘要、补齐差异。
- 注销写墓碑，墓碑永久保留，防止离线节点把删除的记录"复活"。
- 冲突只看版本号；同名不同指纹不冲突，名字只是展示用（`peach#a1b2`）。
- 存活不在名录里，派单前 `GET /health` 现查。

## 测试

```bash
go test ./...        # 单元 + 进程内三节点端到端（含拒绝改派、幂等、墓碑、重启恢复）
./scripts/e2e.sh     # 真实二进制 + CLI 闭环
./scripts/e2e-wake.sh   # 三个容器全在 Docker 里，peach 停止状态，由 apple 用 local 方式唤醒
./scripts/e2e-ui.sh     # 本机页面：内置页、名录脱敏、页面发现、发起工单、CSP
go test -tags integration ./internal/wake -run TestRealLocalDocker   # 真实 docker start
```

## v1 的边界

不做：DAG / 并行 / 汇合（`depends_on` 已预留）、WebSocket、MCP、NAT 穿透与跨网、工单复制（发起者宕机工单即消失）、策略引擎、交付物自动回收、静态加密。

这些不是遗漏，是范围。详细清单见[设计文档第 12 节](docs/全流程设计.md)。
