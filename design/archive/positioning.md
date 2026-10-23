# Anotify Hub · 定位与竞争边界

> 调研日期：2026-10-23。所有外部结论均附链接，未证实的一律标「待验证」。
> 本文回答三个问题：**市场上有谁** · **我们能做而它们做不到什么** · **什么时候不该用我们**。

---

## 1. 一句话定位（三选一，需 owner 定）

| 候选 | 文案 | 优点 | 缺点 |
| :-- | :-- | :-- | :-- |
| **A** | 事件中枢，把任务派到你自己的机器上跑 agent | 直白，说清了形态 | 像基础设施，不像解决方案 |
| **B** | **可解释的自动化：事件为什么触发、走了哪条路、卡在哪一步** | 打在真实痛点上（§3.1） | 弱化了「执行」这个同样重要的能力 |
| **C** | 你的 agent 跑完了，然后呢？ | 情感化，对目标用户（pi/claude-code 用户）直接 | 太窄，装不下事件中心 |

**推荐 B**，理由：可解释性是**唯一同时满足「有市场空白 + 有长期用户痛点 + 我们有架构优势」**的差异点（§3.1）。执行能力作为第二层卖点。

---

## 2. 市场谱系

### 2.1 「中枢 + 用户自己机器上执行」这一族

| 项目 | 是否这个形态 | 执行什么 | 关键限制 | 开源 |
| :-- | :-- | :-- | :-- | :-- |
| **[Windmill](https://github.com/windmill-labs/windmill)** Agent workers | ✅ **最接近** | Windmill 的 job（脚本/flow） | ① **Enterprise 付费**；② **不能跑任意 CLI**；③ 跑不了 flow 状态机，必须留普通 worker；④ 取消任务延迟（按自己的 check interval 才感知） | AGPLv3（Agent workers 属 EE） |
| **[Buildkite Agent](https://github.com/buildkite/agent)** | ✅ 架构范式最接近 | CI job | 工作单元是构建，不是 AI agent | MIT |
| **[actions/runner](https://github.com/actions/runner)** | ✅ | CI job | 官方警告见 §5.3 | MIT（不接受外部贡献） |
| **[Prefect](https://github.com/PrefectHQ/prefect)** / **[Hatchet](https://github.com/hatchet-dev/hatchet)** / **[Celery](https://docs.celeryq.dev/en/stable/userguide/workers.html)** | ✅ worker 常驻拉活 | 用户自己的代码 | 代码永不上传中枢，但 worker 需要「取代码的凭证」——**凭证管理是隐藏成本** | Apache-2.0 / MIT |
| **[n8n](https://docs.n8n.io/deploy/host-n8n/configure-n8n/scaling/enable-queue-mode)** queue mode | ⚠️ 半 | 整条 workflow execution | 前提是 **共享 Postgres + Redis + 同一把加密密钥** | Sustainable Use License |
| n8n task runner | ⚠️ 仅 Code 节点 | JS/Python 片段 | 不是通用节点分发 | 同上 |
| **[Trigger.dev](https://trigger.dev/docs/self-hosting/overview)** | ❌ | 平台自己拉起的容器 | 自托管 = 把整个平台搬过去 | Apache-2.0 |
| **[Inngest](https://www.inngest.com/docs/self-hosting)** | ❌ | 回调你的 HTTPS 端点 | 要求 app **公网可达** | **SSPL**（非 OSI 开源） |
| **[Huginn](https://github.com/huginn/huginn)** / **[Node-RED](https://github.com/node-red/node-red)** | ❌ | 内置 agent 类型 / 节点流 | 没有「在你机器上装进程」这一步 | MIT / Apache-2.0 |

**结论**：`事件中枢 + 自托管节点 + 任意 AI agent CLI` 这个组合**未找到直接竞品**。最接近的 Windmill Agent workers 卡在三处：付费封闭、不能跑任意 CLI、执行单元受限于平台 job 模型。

> ⚠️ 检索局限：GitHub 关键词检索覆盖有限，未穷尽 awesome-selfhosted / Product Hunt / 私有项目。标为**待验证**。

### 2.2 常见对照物

| 产品 | 一句话 | 执行在哪 | 与本项目关系 |
| :-- | :-- | :-- | :-- |
| Zapier / n8n | 把 SaaS 连起来 | 平台服务端 | 相邻，不是同类 |
| Dify / Coze | 编排 LLM 应用对外提供服务 | 平台服务端 | 相邻 |
| Temporal / Argo | 分布式可靠执行复杂 DAG | 你的基础设施 | **刻意不做它的事** |
| RunPod / Modal | 把代码送到云 GPU | 远端 GPU | **方向相反**（它们送代码上云，我们送事件到你的机器） |

---

## 3. 我们能做而它们做不到的 5 件事

每条格式：**证据 → 它们做不到的根因 → 我们的做法**。

### 3.1 「未触发」的逐条件解释 🔴 最强

**证据（n8n 社区与 issue 中的长期高频抱怨）：**

| 现象 | 来源 |
| :-- | :-- |
| 「schedule trigger 配好了、没报错、**工作流就是不启动**」 | [r/n8n](https://reddit.com/r/n8n/comments/1lsh0bw/why_do_n8n_workflows_sometimes_never_initiate/) |
| 「**测试模式完美，生产不工作**」 | [r/n8n](https://reddit.com/r/n8n/comments/1k7tt6k/n8n_workflow_working_perfectly_in_test_mode_not/) |
| 升级后「很多定时工作流不再执行」 | [n8n#31100](https://github.com/n8n-io/n8n/issues/31100) |
| 「IMAP Trigger 停止收信，credential 测试成功、工作流 active、**但连 error execution 都不产生**」 | [n8n#40445](https://github.com/n8n-io/n8n/issues/40445) |
| 用户**读源码**才定位到 `recurrenceCheck()` 会永久丢掉合法触发 | [n8n#23711](https://github.com/n8n-io/n8n/issues/23711) |

**两家最多做到「run 内状态」，做不到「为什么没有 run」：**

- n8n 官方文档只解释 [dirty nodes](https://docs.n8n.io/build/understand-workflows/understand-executions/understand-dirty-nodes)（数据为何 stale），不是条件为何不满足。且 `EXECUTIONS_DATA_PRUNE` **默认 72 小时删记录** —— 事后查都做不到。
- Zapier 只到 run 内 step 状态（`Filtered`/`Skipped`）；触发条件没满足时**根本没有 run**，官方只给 [5 条人工排查清单](https://help.zapier.com/hc/en-us/articles/8496199211277-Zap-isn-t-triggering)。
- Lindy 用户原话：「Loop debugging is difficult/non-existent」。

**它们做不到的根因**：触发是 imperative 的 —— 连上就开跑。触发失败时系统里**没有对象可供解释**。

**我们的做法**：`events` 表存一切，**没命中也有事件**。谓词树求值时收集 trace，于是能给出逐子句结果（蓝图 §5.2）：

```text
rule "rev-pr" against evt_2fQ (github/pr.opened):
  source == "github"                         ✅
  type in [pr.opened, pr.synchronize]        ✅
  attributes.repo == "acme/api"              ✅
  attributes.author not_in [renovate[bot]]   ❌  (author = "renovate[bot]")
  → 未命中。仅因 1 个条件。
```

### 3.2 长时任务无上限 🔴 强

**证据：**

| 来源 | 内容 |
| :-- | :-- |
| [HN 生产总结](https://news.ycombinator.com/item?id=46346023) | 「**30 秒执行上限**逼着把工作流拆分」列为**第一条**生产障碍 |
| [r/n8n](https://reddit.com/r/n8n/comments/1kye4fx/please_increase_timeout_limit_for_deep_research/) | n8n Cloud 对 HTTP Request **强制 5 分钟超时**，而 Deep Research 类 API 要 **30 分钟** |
| [n8n 社区](https://community.n8n.io/t/n8n-self-hosted-ai-agent-node-timeout-after-60s/287731) | 自托管也撞：设了 `EXECUTIONS_TIMEOUT=300`，AI Agent 节点仍 **60s** 报错 |
| [n8n 社区](https://community.n8n.io/t/n8n-workflows-not-respecting-timeout-settings-executions-stuck-for-100-hours/293999) | 反向：配 30 分钟超时，执行卡 **100~200+ 小时不终止** |
| [Gumloop 官方文档](https://docs.gumloop.com/core-concepts/rate_limits) | 低档 **25 concurrent**，非企业档超限**直接 reject 而非排队** |

**它们做不到的根因**：工作流在它们的进程里跑，超时必须设上限，否则资源被占死。

**我们的做法**：agent 跑在用户自己机器上，中枢只关心**租约**。跑 3 小时无压力 —— 中枢每 10 秒收一次心跳。这是架构带来的免费能力。

### 3.3 凭据永不进中枢 🟠 中强

**证据：**

| 产品 | 凭据在哪 | 能否「永远不离开用户机器」 |
| :-- | :-- | :-- |
| **n8n** | 加密后存 n8n 数据库；[加密密钥明文存在 `~/.n8n`](https://docs.n8n.io/deploy/host-n8n/configure-n8n/basic-configuration/configuration-examples/set-a-custom-encryption-key)；queue mode 下**每个 worker 必须持有同一把密钥** | 仅 [Enterprise external secrets](https://docs.n8n.io/administer/manage-credentials/use-external-secret-stores)，且**只是「值从 vault 取」，密钥仍由平台掌握** |
| **Dify** | `providers.encrypted_config` 密文列；每 workspace 一对 RSA，私钥存 `STORAGE_TYPE` 后端 | 仅 `azure-keyvault` 模式私钥不离开 vault（[文档](https://docs.dify.ai/en/self-host/deploy/configuration/environments#key_provider_type)） |
| **我们** | **中枢里根本没有凭据这个字段** | ✅ **结构上不可能持有** |

另一条 n8n 官方警告：env 注入方式「Environment variables aren't protected in n8n, so the data can leak to users.」

**它们做不到的根因**：n8n 能分布式执行（queue mode），前提恰恰是 **共享 DB + Redis + 同一把加密密钥** —— 这三样正是凭据中心化的技术根因。**它不可能既分布式又不持有密钥，因为执行单元需要凭据才能跑。**

**我们的做法**：不是「加密存」，而是**没有这个字段**。节点本地 `argv` 中枢看不到，模型 key 在用户机器上。

一句话文案：**「中枢被拖库，你的模型 key 和私钥也不会泄漏 —— 因为那里根本没有。」**

### 3.4 成本可预测 🟠 中

**证据：**

- [r/aiagents 高分帖（22 分/28 评论）](https://reddit.com/r/aiagents/comments/1r4otc3/unpopular_opinion_zapiers_taskbased_pricing_is/)：Zapier 单月 **$847 / 约 1.5 万 task**，核心论点「**按 task 计费惩罚成功**，业务越大越贵」
- [Composio#4207](https://github.com/ComposioHQ/composio/issues/4207)：托管 API 对某些出口 **约 40% 请求被 429**，且**不带 rate-limit 响应头**、预算还没用完

**我们的做法**：自托管 = 固定成本；模型费用在**你自己的 key** 上，直接可查。

### 3.5 内网 / 私有栈 🟡 弱（不能作核心卖点）

**证据：**

- [n8n 社区](https://community.n8n.io/t/http-node-is-not-working/140308)：Docker 内访问同机 nginx，`host.docker.internal` **永久挂起后 timeout**
- [n8n#21495](https://github.com/n8n-io/n8n/issues/21495)：「Cannot connect local mongo db」with MongoDB Chat Memory node
- [n8n#15785](https://github.com/n8n-io/n8n/issues/15785)：「Ollama Chat Model module is **not using proxy**」
- [n8n 社区](https://community.n8n.io/t/problem-using-ollama-local-ai-agent-in-n8n-1-114-0-after-removing-api-key/224373)：HTTP Request 手动调能通，**Ollama Chat Model 节点却失败**

⚠️ **打折扣的理由**：部分案例是 **Docker 网络问题**，不是平台主动限制（未能确认 n8n 是否主动阻断 localhost）。所以只能作为「顺带优势」。

---

## 4. 不能当卖点的 3 件事（自我纠错记录）

> 这一节是为了防止未来把「行业默认」误当成「差异化」写进文案。

| ❌ 曾以为是卖点 | 实际情况 | 证据 |
| :-- | :-- | :-- |
| **「只出站长连接、无需公网 IP」** | **行业默认**。查了 7 个产品，**无一例外都只需要 agent 出站**：GitHub runner（出站 443）、GitLab Runner（出站轮询，3s）、Buildkite（官方原话「There is no need to forward ports or provide incoming firewall access」）、Windmill agent worker、Prefect（15s 轮询）、Hatchet（gRPC 注册）、Celery（连 broker） | 见各产品官方文档；汇总见调研报告 |
| **「开源自托管」** | 入场券，不是卖点。n8n 早就是 | [HN 老用户原话](https://news.ycombinator.com/item?id=37274764)：「我选 n8n 而非 Zapier 正因为能自托管」 |
| **「能解释为什么没触发」= 我有别人没有** | ✅ **这条成立**（§3.1），但要配合 §5.1 的清醒认识：Windmill 已证明这个形态有人想到，大厂随时可以免费化 | — |

---

## 5. 反面证据（必须内化为产品约束）

### 5.1 Windmill 已经做成了付费企业功能

**好消息**：验证了需求真实存在。
**警告**：大厂随时可以免费化；我们的护城河不能只是「这个形态」，必须是 §3 里那些**架构根因级**的差异。

### 5.2 「可解释性」的实现成本必须守住

我们的可解释性来自 **受限谓词语法**（11 个运算符、谓词树深度 2）。**一旦允许任意代码规则，就同时失去可解释性和安全不变量。**

这条与非目标（蓝图 §1）一致，但值得单独强调：**可解释性是靠放弃表达能力换来的，不是白来的。**

### 5.3 用户机器上的常驻 agent = 高安全风险

GitHub 官方对自托管 runner 的评级极低：

> 「Self-hosted runners ... can be **persistently compromised by untrusted code**」
> 「should **almost never be used for public repositories**」
> —— [secure-use](https://docs.github.com/en/actions/reference/security/secure-use)

**对我们的影响**：我们的「命名命令注册表 + 默认 `allow: false`」只能防**中枢被攻陷**，**防不了 agent 本身被恶意 prompt 攻陷**。

必须补的约束（需写进文档）：

1. 默认 `allow: false`（已有）
2. 节点侧应有**目录白名单**（cwd 必须在允许前缀内，已有）
3. **新增待议**：dangerous 命令（如 `deploy`）是否应默认要求人工批准（蓝图台账 U7 human-in-the-loop）
4. 文档必须明确告知：**不要把 node token 与环境标签一起放进镜像/CI secret**

### 5.4 长期凭证是运维事故高发点

| 案例 | 证据 |
| :-- | :-- |
| GitHub 注册 token **只有 1 小时有效期**，机器时钟偏移会**直接永久退出** | [actions/runner#4648](https://github.com/actions/runner/issues/4648) |
| GitLab 17.0+ 可禁用 legacy registration token，之后 `gitlab-runner register` 报 **410 Gone** | [new_creation_workflow](https://docs.gitlab.com/ci/runners/new_creation_workflow/) |
| Windmill 已改用**短时效 OIDC / workload identity token** 替代长期 token | [agent_workers](https://www.windmill.dev/docs/core_concepts/agent_workers) |

**对我们的影响**：设计里 node token 是**长期 token**（argon2id 哈希）。**建议增加短期 token + 自动续期**（见台账 U28）。

### 5.5 我们的 cancel 比 Windmill 强，应明确写出来

Windmill 官方承认：「An agent worker notices a canceled job **at its own check interval**」（取消是延迟的）。

我们走 WS 帧实时下发 + `kill` 整个进程树（蓝图 §6.1）。**这是可以明确对比的优点**，因为它解决了别人承认的痛点。

---

## 6. 什么时候不该用我们

主动写清这张表，比含糊其辞更能建立信任，也能防止 scope 蔓延。

| 你的需求 | 应该用 | 为什么不用我们 |
| :-- | :-- | :-- |
| 把 Gmail 和 Notion 连起来 | **Zapier / n8n** | 它们有 400+/1500+ 集成，我们只有 2 个适配器 + 通用 webhook |
| 可视化拖拽编排一个 LLM 应用并对外提供 API | **Dify / Coze** | 我们刻意不做画布（成本差 50 倍，且会失去可解释性） |
| 需要真 DAG：条件分支 / join / 补偿事务 / 确定性重放 | **Temporal / Argo Workflows** | 我们只有「事件驱动的多跳派发」，没有 join（蓝图 §8.5） |
| 需要把训练任务送到云 GPU | **RunPod / Modal** | 方向相反：我们送事件到你的机器，它们送代码上云 |
| 团队里没人会用终端 | **Zapier / Gumloop** | 我们要求装 `anotifyd` 并写 YAML |
| 需要 SaaS 级多租户、SSO、审计合规报表 | 商业 SaaS | 本期只留多租户占位（蓝图 §13） |

---

## 7. 定位对产品的 3 个硬约束

如果认可 §1 选 B（可解释的自动化），那么以下三条不能破：

| # | 约束 | 违反的后果 |
| :-- | :-- | :-- |
| 1 | **`POST /v1/events` + 一条 `notify` 规则 = 30 秒出结果**（验收 A1'） | 没有这个最低门槛，所有架构优势都会被「太难用」吃掉 —— 这是 n8n 拖三个框就出结果的地方 |
| 2 | **规则语言永不允许任意代码** | 同时失去可解释性与安全不变量（§5.2） |
| 3 | **中枢永不持有凭据 / 永不下发 argv** | 失去最强的安全差异点（§3.3） |

---

## 8. 待验证

| # | 问题 | 影响 |
| :-- | :-- | :-- |
| V1 | 是否存在未被 GitHub 检索覆盖的同形态项目（awesome-selfhosted / Product Hunt / 私有项目） | 影响「无人做」这个结论的强度 |
| V2 | Composio 当前是否已提供自助 on-prem（[#291](https://github.com/ComposioHQ/composio/issues/291) 关闭后仍有人追问） | 影响 §3.3 的对比措辞 |
| V3 | Gumloop / Lindy 的并发与超时限制官方数字 | 影响 §3.2 的对比精度 |
| V4 | Zapier 是否有企业版「trigger 诊断」能力 | 影响 §3.1 的「无人在做」表述 |
| V5 | Dify `worker_beat` 多副本是否会重复触发定时任务 | 与我们的单实例+租约设计形成对照（若确认，是本项目的额外论据） |
