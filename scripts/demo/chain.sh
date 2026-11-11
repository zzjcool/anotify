#!/usr/bin/env bash
# 多跳链路 demo（v3）—— 已退役（#334，v4 工具链迁移）
#
# 为什么退役：这个脚本整套是 v3 的节点形状——`go build ./cmd/agent`、`node.yaml`
# 的 bootstrap 四件 + `instances`、v2 的「能力产物 → deploy 帧 → 应用」流程、
# `"${ANOTIFY_AGENT_BIN:-anotify-agent}" emit`。v4 删除了 `cmd/agent` 与那条部署链，
# 替换脚本没有意义：留下一个「看起来能跑、其实第一步就红」的脚本比删掉更贵。
#
# v4 的开发链路（都走真实资源，不造假数据）：
#
#   make dev-demo   # devseed 播种能力/源/节点（真 WS hello）+ 通过 /v1/.../events 投事件
#   make dev-node   # 真有台 anode 时：拉能力、执行脚本、回报结果/emit（profile=node）
#   make dev        # 上面两条的宿主版（server + devseed + 可选本机 anode，见 scripts/dev.sh）
#
# 完整 CLI/能力写法见 docs/v4/anode-cli.md 与 docs/design/v4/。
# v3 的原始实现见本文件的 git 历史版本。
echo "scripts/demo/chain.sh 已随 v3 的 cmd/agent 退役；v4 请用 make dev-demo / make dev-node。" >&2
exit 2
