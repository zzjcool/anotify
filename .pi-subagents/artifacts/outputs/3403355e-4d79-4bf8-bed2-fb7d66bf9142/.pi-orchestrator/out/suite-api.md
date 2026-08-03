DONE api_contract

## 产出文件

- `scripts/e2e/suites/api_contract.mjs`（API 契约矩阵套件，48 断言）

## 覆盖 case 清单

### notify 端点（16 断言）

- 无 Key→401；错误 Key→401；recv scope Key→403
- 缺 title→400；空 title(空格)→400；坏 status→400
- 五种合法 status(success/error/interrupted/info/warning)→各 200
- 畸形 JSON→400；超大体(>1MB)→400/413
- deviceTags 归一化（重复/超限/超长）不报错→200
- 无设备用户上报→200 且 matched=0

### vapid-public-key（2 断言）

- GET→200；publicKey 非空

### devices（12 断言）

- 无 session→401；POST 缺 keys→400；POST 合法→200 且返回 device
- GET 列表→200 且含刚 POST 的设备
- PATCH 重命名/statusFilter=error/enabled=false→200；PATCH 坏 statusFilter→400
- DELETE→200；DELETE 后该设备 enabled=false 或消失

### keys（8 断言）

- 无 session→401；POST→200 且 ant_ 前缀明文；POST 无 scopes→400
- GET→200 且不含明文 key；revoke→200；被 revoke Key 上报→401

### notifications（5 断言）

- 无 session→401；上报 2 条后 GET→count≥2；limit=1→count=1；sinceSeq 分页→更少

### 静态/缓存（5 断言）

- /→200；index.html Cache-Control 含 max-age=60；/v1/* 含 no-store
- manifest.json 存在且为合法 JSON；取一条哈希资源→Cache-Control 含 immutable

## 自测命令与结果

```bash
ANOTIFY_VAPID_PUBLIC_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['publicKey'])") \
ANOTIFY_VAPID_PRIVATE_KEY=$(python3 -c "import json;print(json.load(open('vapid.json'))['privateKey'])") \
node scripts/e2e/suites/api_contract.mjs
```

→ **48 通过 / 0 失败，exit code = 0**（已二次确认 exit code）

## 发现的产品 bug

- 无。所有端点行为与公共约定/任务卡文档一致（含 revoke 后 401、Key 不明文落库、缓存分级正确）。

## 遗留风险

- 无阻塞。两点备注：
  1. `keys POST` 返回的 `record.id` 字段在 Go 结构体是 `ID`（大写），JSON 序列化保持 `ID`；套件已兼容 `record.ID ?? record.id` 两种形态，稳健。
  2. 超大体断言接受 400 或 413（MaxBytesReader 实际返回 400），行为正确。

## 备注

- 未改底座/他人套件，仅新增本套件文件。
- 工作区另有 edge_cases/persistence/push_e2e/ws_protocol 等 suite 文件为其他子 Agent 产出，与本任务无关。
- 无 staged 文件。

```acceptance-report
{
  "criteriaSatisfied": [
    {
      "id": "criterion-1",
      "status": "satisfied",
      "evidence": "仅新增 scripts/e2e/suites/api_contract.mjs，覆盖任务卡列出的全部 case（notify/vapid/devices/keys/notifications/静态缓存），未改底座或他人套件，未扩大范围"
    },
    {
      "id": "criterion-2",
      "status": "satisfied",
      "evidence": "自测 48 通过 / 0 失败，exit code=0（二次确认）；附完整命令、覆盖清单、产品 bug 结论与遗留风险"
    }
  ],
  "changedFiles": [
    "scripts/e2e/suites/api_contract.mjs"
  ],
  "testsAddedOrUpdated": [
    "scripts/e2e/suites/api_contract.mjs"
  ],
  "commandsRun": [
    {
      "command": "ANOTIFY_VAPID_PUBLIC_KEY=... ANOTIFY_VAPID_PRIVATE_KEY=... node scripts/e2e/suites/api_contract.mjs",
      "result": "passed",
      "summary": "48 通过 / 0 失败"
    },
    {
      "command": "同上传 stderr/stdout 丢弃后 echo $?",
      "result": "passed",
      "summary": "exit code = 0"
    }
  ],
  "validationOutput": [
    "[api_contract] 48 通过 / 0 失败；exit code = 0；覆盖 notify 鉴权/参数校验、devices CRUD、keys 生命周期、notifications 分页、静态缓存分级全矩阵"
  ],
  "residualRisks": [
    "keys POST 返回 record 的 id 字段 Go 侧为大写 ID，套件已兼容 record.ID ?? record.id，无风险",
    "超大体(>1MB)断言接受 400 或 413，实测为 400（MaxBytesReader），行为正确"
  ],
  "noStagedFiles": true,
  "diffSummary": "新增 scripts/e2e/suites/api_contract.mjs（~190 行）：基于 harness 的 API 契约矩阵套件，48 条断言",
  "reviewFindings": [
    "no blockers"
  ],
  "manualNotes": "所有端点行为与契约一致，未发现产品 bug。工作区其他 suite 文件（edge_cases/persistence/push_e2e/ws_protocol）为并行子 Agent 产出，与本任务无关。"
}
```
