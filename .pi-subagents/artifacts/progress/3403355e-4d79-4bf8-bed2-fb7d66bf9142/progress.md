# routing.mjs 进度

- [x] 读公共约定 + 任务卡 + harness + 产品代码（route/handlers/store）
- [x] 编写 scripts/e2e/suites/routing.mjs（设备拓扑 A/B/C/D + 8 大路由 case + 配置生效预检）
- [x] 自测跑通（exit 1）——**正确暴露产品 bug：PATCH /v1/devices/:id 无效**
- [ ] 等待协调者修复 store.UpdateDevice + patch()（方案 A，协调者亲自修）
- [ ] 修复后重跑至 exit 0

## 当前状态

套件已写好且行为正确：1 通过 / 22 失败，全部失败指向同一根因（PATCH 不落库，
设备全部停在默认 tags=[]/filter=all/enabled=true → 所有消息广播到全部 4 台）。
待产品修复后重跑。
