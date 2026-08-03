# 任务：编写 E2E 套件 `scripts/e2e/suites/api_contract.mjs`

先读 `/Users/zheng/code/anotify/.pi-orchestrator/tasks/e2e-suites-common.md`（公共约定）。

写 API 契约矩阵套件，覆盖以下全部 case（用 H.seed 建用户拿 sendKey/recvKey/session）：

## notify 端点

- 无 Key → 401；错误 Key → 401；recv scope Key → 403
- 缺 title → 400；空 title(空格) → 400；坏 status → 400
- 合法 status 五种(success/error/interrupted/info/warning) → 各 200
- 畸形 JSON → 400；超大体(>1MB) → 400 或 413
- deviceTags 归一化：重复 tag 去重、>10 个截断、>32 字符截断（上报后 matched 或 200 即可，归一化逻辑在 Go 单测已覆盖，这里主要验证不报错）
- 无设备用户上报 → 200 且 matched=0

## vapid-public-key

- GET → 200 且 publicKey 非空

## devices（需 session）

- 无 session → 401
- POST 缺 keys → 400；POST 合法 → 200 且返回 device
- GET 列表 → 200 且含刚 POST 的设备
- PATCH 重命名 → 200；PATCH statusFilter=error → 200；PATCH 坏 statusFilter → 400；PATCH enabled=false → 200
- DELETE → 200；DELETE 后 GET 列表该设备 enabled=false 或消失

## keys（需 session）

- 无 session → 401
- POST {name,scopes:[notify:send]} → 200 且 key 以 ant_ 开头
- POST 无 scopes → 400
- GET → 200 且不含明文 key
- POST /v1/keys/:id/revoke → 200；用被 revoke 的 Key 上报 → 401

## notifications（需 session）

- 无 session → 401
- 先 seed 用户用 sendKey 上报 2 条 → GET ?limit=50 → 200 且 count≥2
- limit=1 → count=1；sinceSeq 分页 → 返回更少

## 静态/缓存

- / → 200；index.html Cache-Control 含 max-age=60；/v1/health 或 /v1/* Cache-Control 含 no-store
- manifest.json 存在且为合法 JSON；取其中一个哈希资源请求 → Cache-Control 含 immutable

自测跑通（exit 0）后上报。
