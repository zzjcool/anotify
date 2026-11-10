# anode：v4 独立节点安装与运行

anode 只有 `serve`、`install`、`emit` 三个公开子命令。没有 `run`、旧节点配置或旧命令别名；不写子命令也不会隐式启动。`anode --version` 输出单行 `{"version":"…","commit":"…"}`，hello 和升级 ACK 使用同一构建事实。

## 1. 先创建长期节点身份

管理员先在中枢调用 **POST Node → POST token**（v4 管理 API），保存返回的 Node 名称、server 地址和长期 Node identity token。明文 token **只返回一次**；后续管理响应不能取回它。身份属于节点，不是某次任务的 attempt token。具体管理资源路径/权限以中枢 API 文档为准。

`install` 必须显式提供这个已有身份：不注册、不生成 token、不安装第二个注册服务、不自动部署能力。不要把长期凭证放进脚本、镜像或公开日志。推荐临时保存为仅安装用户可读的 0600 token 文件，安装后由管理员按自己的凭证管理策略处理；程序不自动删除用户文件。

```sh
./anode --version
./anode install --name node-a --server wss://hub.example.com \
  --token-file /secure/node-token.txt
```

无 stdin、无 sudo、无默认 `systemctl` 调用。写文件权限不足会明确失败，需管理员以合适身份重跑原命令。成功输出管理员后续步骤，而不是擅自操作宿主服务：

```sh
systemctl daemon-reload
systemctl enable --now anode.service
```

默认安装到：

| 文件 | 默认路径 / 权限 |
|---|---|
| 二进制 | `/usr/local/bin/anode`，0755 |
| strict 配置 | `/etc/anode/anode.yaml`，0600 |
| systemd unit | `/etc/systemd/system/anode.service`，0644 |
| 私有数据目录 | `/var/lib/anode`，新建目录 0700 |

unit 用 `serve --config … --data …`，`UMask=0077`，`Restart=always`。它不会导入一个全宿主环境文件。若需要凭证变量，管理员通过自己的受保护 systemd 配置显式注入，并同时在 anode.yaml 白名单声明名字。

### 重装与凭证更新

同名/server/token 重装保留用户现有 namespaces、vars、limits、listen、envPassthrough，甚至不改原配置字节。不同 token 默认拒绝；只有显式 `--update-token` 才更新保存的 token，保持所有本机配置值。换 name/server 视为不同身份，拒绝覆盖。已有未知/非法配置、修改过的 unit、非本安装的已有二进制会明确拒绝，不删用户数据目录。单文件采用同目录临时写、fsync、rename，失败不会留下半个 YAML/二进制；安装涉及多个文件，不声称跨文件事务，权限问题处理完可以安全重跑。

```sh
./anode install --name node-a --server wss://hub.example.com \
  --token-file /secure/rotated-node-token.txt --update-token
# 随后管理员重启 service，使新 token/本机配置生效。
```

`--prefix /absolute/staging` 把默认布局置于 staging 根内；`--config`、`--bin`、`--unit`、`--data` 可显式覆盖路径。prefix 非根、路径必须绝对规范，无 `..`、符号链接、特殊文件或 prefix escape，三个文件名仍必须是 anode.yaml、anode、anode.service。staging 不启动/改动宿主服务；输出如需复制 unit 的管理员步骤。目录应由管理员控制，这是安装安全边界，不是对恶意本机并发 writer 的 sandbox。

## 2. 下载引导：同一个安装器

```sh
# 建议先下载并检查脚本，而不是执行未审核内容。
curl -fsSL https://example.com/releases/install-anode.sh -o /tmp/install-anode.sh
sh /tmp/install-anode.sh --version v4.0.0 -- \
  --name node-a --server wss://hub.example.com --token-file /secure/node-token.txt
```

`scripts/install-anode.sh` **只下载、核对 checksum/版本/架构、调用下载二进制的 `anode install`**；不写 YAML/unit，不 sudo，不 stdin，不自动部署。发布侧提供 `anode_linux_amd64` / `anode_linux_arm64` 和对应 `.sha256` 单行摘要，由 T1 管理 release 产物。显式版本必需；架构默认来自 Linux uname，可用 `--arch amd64|arm64` 指定。可用 `--download-url`、`--checksum-url` 或 `ANODE_DOWNLOAD_URL`、`ANODE_CHECKSUM_URL` 指向离线镜像；默认基址可通过 `ANODE_DOWNLOAD_BASE` 替换。`ANODE_VERSION`/`ANODE_ARCH` 也可设置。下载有 10s 连接/60s 总超时，checksum 不符或二进制 `--version` 不符不会调用 install；install 的非零退出码原样传回。

checksum 保证完整性，不是签名/可信分发证明；下载源与摘要源必须可信。引导进程不输出 token，但 install 的 `--token` 本身会出现在进程 argv，优先 `--token-file`。

## 3. serve 与严格配置

```sh
anode serve --config /etc/anode/anode.yaml --data /var/lib/anode
```

`serve` 只支持这两个路径 flags（以及 help）；listen/env 行为来自 **strict anode.yaml**，没有旧 `node.yaml` 接口。未知字段/多文档/重复或非法白名单/空 namespaces/非正 limits 启动失败，不半在线。SIGINT/SIGTERM 触发 drain，终止授权、TERM→grace→KILL 真进程组后关闭网关和连接。缺应用配置时不会假装在线。

server 空 path 补共享 `/v1/node/stream`；显式反代 path 原样使用。合法示例（不要把真实 token 提交到版本库）：

```yaml
name: node-a
server: wss://hub.example.com
token: "REPLACE_WITH_EXISTING_NODE_IDENTITY_TOKEN"
namespaces: [default]
listen: tcp://127.0.0.1:0            # 默认仅本机、随机实际端口；可用 unix:///run/anode/emit.sock
vars:
  SOURCE_ROOT: /srv/project
limits:
  total: 2
  perCapability:
    review: 1
envPassthrough:
  - API_KEY
```

namespaces 是申报，与中枢授权取交集。机器值 local > node，下游 ns defaults 只在任务内存中合并，不落 applied。修改本机 YAML 需重启。能力仅由管理员 **manual deploy** 请求：Node Bearer GET `/v1/node/capabilities/{digest}` 返回直接 Artifact JSON（无 wrapper），验证 refs/spec/digest 后原子缓存；install 与重连都不是自动部署。

### 环境和宿主权限（不是 sandbox）

任务只拿基础 ANOTIFY_ 变量、params、明确 run.env 和宿主 **名字白名单**。PATH 固定为节点进程启动基线，命令探测同源；禁止 run.env/passthrough 覆盖 PATH。HOME 来自节点基线/明确 run.env，必须是存在的绝对目录。白名单 ≤32 名，拒绝重复/非法/PATH/HOME/ANOTIFY_；run.env 优先于同名白名单值。凭证值不进入 hello/applied/hash/诊断；诊断只报告实际名字与来源。

凭证不是沙箱：脚本以 service 用户的宿主权限执行，仍可访问该用户可读的文件、网络、工具和挂载。选择最小权限服务用户，并确保它对指定二进制目录具有升级所需权限；程序不会运行 sudo 或创建新的权限隔离产品。

run.files 普通文件写入 0600，entrypoint 0700，直接按 shebang exec，不包额外 shell。用户 workDir 必须事先存在、绝对规范，禁止越界/符号链接逃逸；节点不创建它。**同名文件会被覆盖且权限改变**，包括用户原有同名文件；明确选择专用 workDir 或接受这个副作用。节点绝不删除用户 workDir/其祖先，TTL 不构成用户目录清理许可。

## 4. 脚本 emit

```sh
anode emit review --id logical-report --data '{"summary":"ready"}'
anode emit --to review --data '{}'
```

目标可以位置参数或 `--to`（矛盾目标明确失败）；只有 `--id`、`--data`，data 必须是 JSON object。token 只从 `protocol.EnvTaskToken`（`ANOTIFY_TASK_TOKEN`）、端点只从 `protocol.EnvEmitEndpoint`（`ANOTIFY_EMIT_ENDPOINT`）读取；token 只放 Authorization header，**不进 argv/query/payload**。TCP 使用节点实际端点；`unix://path` 自定义 HTTP transport 请求 `/emit`。网关经唯一 E1 task.emit/task.emit.ack WS 向中枢请求，只有中枢授权/幂等/建下游是权威。

| 退出码 | 判据 |
|---|---|
| 0 | HTTP 200 已接受，包括 taskId:null + eventId/pruned 的剪枝 |
| 2 | 缺目标/非法参数/HTTP 400 |
| 3 | 缺/无效 attempt token、HTTP 401 |
| 4 | HTTP 409 task_not_running |
| 6 | HTTP 409 emit_id_conflict（同逻辑 id 异 body） |
| 5 | 超时、断线、未知结果，**可能已派发** |

结果 stdout 一行 JSON；错误另 stderr 一行人话。不自动重发请求，尤其不无 id 重发。相同 logical task+显式 id+同 body 可跨 attempt 返回原 task 或原剪枝 event；异 body 409。旧 token 必须先被拒，不能靠已知 id 取回旧权威授权。无 id 两次是两个不同投递。`emit` 退出 5 是脚本结果未知，**不得**当 whole-task infra 自动重跑的依据；想安全重发，由脚本显式使用同一个 id/body。

## 5. 每 attempt 导出与 A1 上行

A2 为每 `(taskId, attempt)` 新建系统自有唯一 0700 导出根，注入 `ANOTIFY_ARTIFACT_DIR`。任务重试/并发共享 workDir 不共享导出根；不要依赖固定工作目录或 stdout 魔法：

```sh
#!/bin/sh
set -eu
printf '%s\n' 'report bytes' > "$ANOTIFY_ARTIFACT_DIR/report.txt"
mkdir "$ANOTIFY_ARTIFACT_DIR/details"
printf '%s\n' 'detail' > "$ANOTIFY_ARTIFACT_DIR/details/output.txt"
anode emit review --id report --data '{"summary":"ready"}'
```

空导出目录合法 `[]`；创建/注入/枚举失败、symlink/特殊文件是明确 artifact_export_invalid failed，不假空成功、不自动重试。用户 workDir 内旧目录不创建、不删除、不因存在拒绝。

完成后安全枚举→托管副本→sha256→A1 出站 PUT raw bytes `/v1/node/tasks/{taskId}/artifacts/{sha256}`，携 Node Bearer 与 attempt/lease/task-token/编码逻辑 path headers。201 首次、200 同 path+hash 幂等，异 hash409；最多32项/单16MiB/attempt64MiB。上传期间继续续租；只有授权仍有效才对同 path/hash 有界重试，不 whole-task replay。确定上传失败为 artifact_upload_failed，保留已经成功上传的清单，不能把未知上传伪成功/空数组。用户下载授权/过期410由中枢负责，节点不开放用户文件回源。

GC 默认7天，grant/冻结 artifact 的 resultTtlSec 可覆盖，**0 永不按时间删**。只清系统创建的 attempt/work/export/日志/托管副本；活动/未确认 reaped 槽位不清；未知 meta 不删；用户原目录、原文件和祖先永不删。

## 6. 自升级与容器

升级是 drain→确认物理组 reaped（历史 lease/slot ACK）→同源 Node Bearer 下载 raw binary→sha 校验→下载二进制实际 `--version` 自检→同目录 atomic replace→ACK **实际 version+commit**→真实 exec 同 argv/service env 重启→新 hello。自检输出有界/10s超时，环境只有 PATH/HOME，不继承任务 run.env 或宿主凭证。bad_sha/自检失败不替换、不重启；目标目录权限不足明确 `need_root`，不把 Linux 一概伪装成 not_supported。不确认 reaped 不释放真实物理槽位、不升级；缩预算也保留活存量。升级失败时已经进入 drain，不自动恢复派发或重跑旧任务；管理员修复下载摘要/权限后重启 serve 恢复接任务。

```sh
docker build -f Dockerfile.anode --build-arg VERSION=v4.0.0 \
  --build-arg COMMIT=RELEASE_COMMIT -t anode:v4.0.0 .
docker run --name anode --restart unless-stopped \
  -v /secure/anode-config:/etc/anode:ro \
  -v anode-data:/var/lib/anode \
  -e API_KEY=EXPLICIT_VALUE anode:v4.0.0
```

Dockerfile 独立 CGO=0 Go 二进制，无 SQLite dependency closure，默认 serve，仅配置默认 local emit，不发布端口、不导入宿主环境。配置/数据卷显式提供；`-e API_KEY` 和 config 白名单需同时具备。镜像默认进程用户权限不是 sandbox，必要时用管理员选择的 `--user` 和正确卷权限。容器内二进制目录只读/无写权限时 self-upgrade 明确失败，应由管理员更新镜像重启，不能伪成功。构建需外部 Go/Alpine/CA 网络；网络失败不是镜像构建 PASS。

## 7. 验证映射

具名测试和原始命令/结果见 `teams/anode/N3-TESTS.md`。安装测试全 temp/offline，无宿主 service/binary 修改；脚本测试 fake curl 真复制并 fake binary 真转发/失败；能力脚本测试执行实际构建 anode emit，含跨 attempt 回原 task/pruned、异 body conflict、旧 token 先拒、无 id 不同投递；升级测试复制真实旧 binary，在 fake hub 下替换当前进程的文件、真实重启并上报新 hello，不用空 Restart 冒充。
