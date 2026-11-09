// Package protocol 是 v4 节点协议（中枢 ↔ anode）的唯一类型源。
//
// 为什么单独一个包：帧、枚举、错误码、emit 网关 DTO、applied 文件形状都只能有
// 一份定义。中枢（internal/ws、internal/server）与节点（cmd/anode）都 import 本包，
// 禁止各自再写一份 snake_case / camelCase 的镜像类型——两份镜像第一次分叉时，
// 症状通常是「节点读不到中枢刚加的字段」，而两边都编译通过。
//
// 三条纪律：
//
//   - 零本项目依赖：只用 stdlib（D-I10）。节点二进制不链 sqlite / server。
//   - wire 字段一律 camelCase（docs/design/v4/api-conventions.md §3），帧类型名是
//     文档里的点号名（task.dispatch / config.applied …），字符串本身是协议。
//   - 无兼容层：v4 不提供 v3 帧别名。旧字段就是未知字段，解码时被忽略而不是报错
//     ——一条老帧不该因为多了一个退役的键就整帧被拒。
//
// 构造帧时，必填的列表/映射要给空值而不是 nil（空列表纪律：wire 上是 []，不是
// null）。本包不做「自动补空」：帧的字段就是它声明的东西，隐式补全会让「节点没报」
// 和「节点报了个空」分不开。
//
// 帧集合 = 配置 5 帧（config.md §4.2）+ 执行 10 帧（capability.md §8.5）+ 保留的
// 运维 4 帧（upgrade / upgrade_ack / logs_request / logs_response）。
//
// 本包不含传输与会话状态机：ws 只做收发、心跳与背压；派发、租约、结果、emit 的
// 权威判定都在 route/store。
package protocol
