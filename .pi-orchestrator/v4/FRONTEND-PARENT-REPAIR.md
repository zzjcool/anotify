# 前端完整195组后的精确返修（父）

冻结6364b92/current7180e345的五套件全部实际执行：174PASS/21FAIL，源码前后一致、每ID一次。完整summary在 `/tmp/anotify-v4-fe-final-gates/full-five-summary.json`；三类原log/attempts保留，不重新使用final-08af的ID。

## 已确认有限根因

1. observe-errors：f3Driver导航initScript清空__f3Fault，故障case后goto实际返回200正常图五按钮。修test故障注入的跨本case导航生命周期，实际故障HTTP/错误容器/唯一retry必须都断言，不能放宽5!=1或改生产重试UI。
2. playbook-delete：删除后retention helper仍查询父包依赖installs分组列表，404不等于资源/回执级联。删除前保存精确receiptId，删除后验证独立单回执、Capability/Source与实际执行保留；公开已删包404不变，不恢复包仅为helper绿。
3. i18n/coverage16组重复失败去重为五producer叶：Capability displayName、Key name、Source列表displayName、Source详情displayName、Event原始JSON含诊断/载荷。只读确认全部为真实数据，未证UI固有词缺译。依既有L10n规则，仅纯文本数据SPAN标raw，保名称原字和HTML安全/现敏感值处理。PRE/H2/A/DIV整块不豁免，周边UIchrome仍检查、加载/错误提示不残留marker。

## 几何3项尚需取证

- prelayout expanded/collapsed均page-main内层盒比sidebar右界差32，实际mountLayout把它放带sm:px-8的outer main。当前case取挂载后内层x不是prepaint外界。需补outer/content/pre-mount时点真实盒证据后对正确既定边界断言，不hardcode+32或去掉预布局保护。
- namespace-switch只URL/storage已变就量slot，saveScope实际reload，report缺任何box/同DOM身份。需跨文档/slot/header/nav/文本/请求记录确定是错误采样还是生产postpaint丢槽，不能延frames等稳定或接受0/降低40阈值掩盖。

## 第一单writer

明确接管已safe停的旧FEWT，不新manager重复恢复。只修前3类及补精确几何证据（不未经证据改geometry生产），保原完整195/304矩阵和180Node。

owner：五数据producer页、test故障driver/观察/包保留helper、原Node回归测试及只读几何case诊断、团队父返修报告。生产partials/layout/CSS/模型/认证/图/Go/API禁止写，几何新证据若需这些路径父再精准批准。

所有原full失败记录留在未跟踪的三runs目录，不混入新commit或清理。修改后重新makefe/当前stage tests/新隔离验证run使用不同诊断ID；旧READY/full epoch保留历史，不改旧receipt。全部已知根因清后fresh门槛+新READY再完整五套件各一次，全产品真实CoreNodecross仍独立。
