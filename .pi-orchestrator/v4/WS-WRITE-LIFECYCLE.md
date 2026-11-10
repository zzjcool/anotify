# Node 共享 WS 写生命周期与可信结果 ACK（父精准裁决）

父固定e64独立全包复验再现App logs EOF，新的传输诊断捕获首Close来自coder/websocket.setupWriteTimeout的context.AfterFunc；不是测试收尾，发生在Task.result已收到、可信ACK和logs请求之间。源链已核：结果发送继承attempt uploadCtx，可信resultACK取消该ctx；WebSocket Write结束前取消回调仍可能活动，进而关闭整个共享连接。具体历史发生的cancel来源仍由确定性反例/时序诊断闭环，不因一次PASS称稳定。

## 两个线性化边界

1. **发送准入**：取得writeMu后、实际conn.Write前，仍需检查当前连接generation、pending不可变绑定、lease授权/截止与caller取消。此前任何cancel/expiry/replacement必须不发送。
2. **已准入的实际写**：使用该共享连接的写生命周期及有界write deadline（不晚于准入时原lease snapshot所给截止）。不能继续继承attempt uploadCtx的取消传播，让可信ACK的正常完成清理杀死共享WS。

- 明确允许：越过发送准入点后的caller cancel不立即取消这一个已开始的WS Write；该写仍受原截止/固定write timeout/连接关闭约束，服务端最终当前授权校验决定是否接受。已经发送的字节不能撤回，不承诺通过ctx取消逆转。
- 真cancel/expiry的进程终止、HTTP上传ctx取消、旧pending禁后续重发保持立即生效；这不是延长lease/复活execution或允许新emit。
- 成功resultACK可立即停止该attempt的续租、释放匹配的结果资源，不等Write/ACK互相等待或延迟清理。共享WS不能因该正常清理关闭，其他任务续租/日志继续正常。
- 连接主动关闭/Run结束仍可终止实际写。不得创建永不截止的Background发送或去掉10秒write上限，不持runner/attempt锁等待网络或同步ACK。

## 最小实现范围

允许当前Node唯一writer修改hub.Client.SendGuarded写ctx生成与必要runner.sendResultFrame接缝（纯guard仍保当前授权）；可使用取消分离后恢复原deadline、连接自身Close传播的等价最小实现。无需新frame、ACK字段、任务生命周期框架或第三方库修改。

确定性反例：真实frame已经送到receiver，暂停sender Write返回/回调卸载；receiver发可信ACK并触发正常attempt cleanup，旧实现整个conn关闭、新实现不会因此关闭；放行后同conn logs和其他正常帧可读。另保queued cancel/expiry无旧字节、真正HTTP upload撤权、原write deadline/connection close、resultACK同步callback不死锁及新旧slot绑定。

在上述线性化点之后关闭caller ctx与写ctx取消不再是同一事件，是原『已开始写不可撤回』规则的明确实现，不降低前置guard。全原Node/protocol/artifact、重复fullchain、race和最后fresh限域门槛通过前仍不交Node最终支持。
