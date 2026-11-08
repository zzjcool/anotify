# Playbook Spec v4 · 安装包

> 状态：**讨论中**（2026-11-08）。标 ⚠️ 的是待定项。
> 配套：`capability.md`（能力）、`source.md`（源）。

---

## 0. 定位

**预案 = 一个可导入的安装包**：里面是一组**能力** + 一组**源**。

三条性质：

1. **自包含**：包内能力与源都是**内联定义**，不依赖外部文件就能装出完整的图。
2. **不是运行时对象**：装完之后用户面对的是「能力」「源」这些一等对象；预案只是它们的**来源清单**（从哪儿来、由谁装的）。
3. **不含边**：v4 的边就是能力的 `next`，跟着能力一起进来，没有独立的边声明。

---

## 1. YAML 形状

```yaml
apiVersion: anotify/v1
kind: Playbook

metadata:
  name: review-chain          # 包名（命名空间内唯一）
  displayName: 代码审查链
  version: 1                  # 包自己的版本，便于人看；不参与能力的 digest

spec:
  capabilities:               # 内联能力 = Capability 文档去掉 apiVersion/kind
    - metadata:
        name: review
      spec:
        params:
          type: object
          properties: { repo: { type: string } }
          required: [repo]
        next: [fixer]
        run:
          entrypoint: run.sh
          files:
            run.sh: |
              #!/usr/bin/env bash
              anode emit fixer --data '{"file":"a.go"}'

    - metadata:
        name: fixer
      spec:
        params:
          type: object
          properties: { file: { type: string } }
          required: [file]
        next: []
        run:
          entrypoint: run.sh
          files:
            run.sh: |
              #!/usr/bin/env bash
              sed -i 's/TODO/DONE/' "$ANOTIFY_PARAM_FILE"

  sources:                    # 内联源 = Source 文档去掉 apiVersion/kind
    - metadata:
        name: on-push
      spec:
        kind: webhook
        enabled: true
        config: {}
        targets:
          - capability: review
```

**内联项与独立文档是同一个形状**（只是少了 `apiVersion`/`kind`）。一个解析器、两种入口，
不再出现 v3 那种「ref / 内联 spec / v1 旧形态」三种互斥写法。

---

## 2. 引用关系

```yaml
next: [fixer]        # 包内已有 fixer → 装完就连上
next: [some-existing] # 库里已有、包里没有 → 允许（装完由 status.DownstreamResolved 判定）
```

- **包内**：允许直接引用，安装时在同一个事务里建，顺序无所谓（一次性写入，不存在先后）。
- **包外**：⚠️ **待定 D5** —— 允许引用库里已有的能力（现在允许），还是要求引用必须自包含？
  允许的好处：多个包共用同一个能力；坏处：包不是自包含的，`DownstreamResolved` 可能装完就是 false。

---

## 3. 安装语义

沿用现在这套（它们是对的）：

| 规则 | 说明 |
|---|---|
| **单事务** | 能力、源、边一起写，要么全成要么全不成；不存在「装了一半」 |
| **创建期校验** | 引用不到的名字、参数不合法 → **建包时** 400，不是装的时候 skip（配置期是唯一能把矛盾说清楚的机会） |
| **幂等** | 重复安装不产生重复行：同名能力 → 跳过，同名源 → 跳过，同样内容 → 复用 |
| **跳过必有原因** | 只有「缺定义 / 同名源已存在 / 同内容已是别的版本」这类才写 note；「早装过了」不写 |

```yaml
# 安装回执
created:  { capabilities: [review, fixer], sources: [on-push] }
skipped:  { capabilities: [],             sources: [] }
notes:
  - "源 on-push 已存在（kind=webhook），跳过"
```

⚠️ **待定 D1（最关键）**：**包里的能力与库里同名时怎么办？**

现在：跳过。但 v4 的能力带 `next`——如果用户已经手动建了 `review` 且 `next: [other]`，
再装一个 `next: [fixer]` 的包，跳过就等于**用户以为装上了、实际没接上**。

三个选项：

| | 行为 | 风险 |
|---|---|---|
| 跳过（现状） | 库里那份不动 | 「装完却不生效」的静默失败 |
| 覆盖 | 用包里的定义更新（新版本或原地改 next） | 覆盖用户手改的东西 |
| **拒绝** | 同内容不同 → 装失败，列出冲突 | 最吵，但绝不会静默不一致 |

---

## 4. 版本

能力的 `version` 省略 = 内容寻址（见 `capability.md §10-B`）。

⚠️ **待定 D2**：包里能力的 `version` 是**省略**（跟内容走）还是**必须显式**（包 = 一份可复现的清单）？

- 省略：改一行脚本再装，自动成为新版本，不需要人管号
- 显式：同一份包装到任何地方都是同一批版本号，但每次改都要人加号

⚠️ **待定 D3**：包升级（同名包改内容再装）如何表达——新 `metadata.version`？还是重装即升级？

---

## 5. 卸载

现在：**删预案不回滚已装的能力与源**（它们已经是独立对象，有别的引用者）。

⚠️ **待定 D4**：v4 要不要支持卸载？

- 不支持：与现在一致，删预案只是删清单
- 支持：但必须定义「有别的上游引用它时怎么办」——能力是图的一部分，删一个可能断好几条链

---

## 6. 与 v3 manifest 的差异

| v3 | v4 |
|---|---|
| `capabilities[]` 三种写法（`ref` / 内联 `spec` / v1 旧形态） | **只有一种**：内联（形状 = Capability 文档） |
| `edges[]`（含 `fromPort` / `literals` / `priority` / `mapping`） | **整段删除**：边就是能力的 `next` |
| `triggers[]` | 改名为 **`sources[]`**，形状 = Source 文档 |
| `{ref: "x@2"}` | 删除。要引用已有能力就在 `next` 里写名字 |

---

## 7. 待定清单

| 编号 | 问题 | 影响的读者 |
|---|---|---|
| **D1** | 包里能力与库里同名时：跳过 / 覆盖 / 拒绝 | 安装语义（最关键） |
| **D2** | 包里能力的 `version` 省略还是显式 | 可复现性 |
| **D3** | 包升级怎么表达 | 版本管理 |
| **D4** | 要不要卸载 | 生命周期 |
| **D5** | 允许引用包外的能力吗 | 自包含性 |
| **D6** | 源同名已存在时：只跳过，还是允许更新 `config`（比如轮换 webhook token） | 运维 |
