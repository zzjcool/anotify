# 结果提交授权：本地超时与租约撤销（父精确澄清）

用于T3 guardedSend/leaseLoop最后返修，不改变D4/A1/A3、不加wire或权限框架。

## 本地timeoutSec

- timeoutSec是能力进程本地执行期限，触发时终止进程组并真实回收，保存failed/timeout原因与原finishedAt。
- 只要中枢lease有效，继续20秒renew和ACKdeadline用于安全导出、上传、合法failed结果发送与可信result.ack确认；不能把procDone/terminating/localtimeout等同leaseOK=false。
- 保留finalization授权不表示重启进程、接受新的脚本emit或放宽已取消/已过期授权。进程生命周期与结果完成生命周期分开。
- `LookupEmit` 的准入判定必须同时检查有效lease/session与本attempt的 `emitOpen`；不能只看保留用于finalization的授权。进程已停且emitOpen=false、token映射尚未删除的窗口也拒绝新emit，但合法failedtimeout结果仍可提交；准入点之前已开始的在途请求最终仍由中枢当前授权事务决定，不虚称可撤回已发送字节。

## 真正cancel / leaseexpiry / 身份失效

- 授权撤销后停上传，guard禁止发送无授权结果；本机归档保存实际失败，不为了发结果恢复租约或返回假成功。
- 历史reaped通知沿Node当前身份+task/attempt/lease/slot实际绑定安全回收，不要求旧execution仍running；不能清新占用或复活旧emit/result。
- 旧测试若要求生产runner发送失授权task.result，应精准改成无发送+归档/真reap/当前新pending不动。服务端late非法frame拒绝测试仍保留，可主动注入，而非逼生产代码违反guard。
- 错/旧grant未合法执行不生成task.result，忽略告警且不能清新reservation；合法唯一grant的结果一直绑定到原lease，ACK不能从新current随意补身份。

## 必测与验收

真实timeout终止→上传/发送等待跨20/60秒仍续租→合法failed结果ACK后终结；期间真expiry/cancel禁止latewrite并停上传；延迟ACK不释newslot；旧expiry结果本机保存/历史回收和服务端拒非法frame；mismatchgrant不exec/不污染newpending。

旧原三工具门禁和affectedrace、限域独立review不减少；日志失败或超时不能假归infra以自动整任务重跑。
