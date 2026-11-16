# Trace启动图库依赖：真实现场与最小修复授权

## 已取得真实失败（不是再次猜测）

观察run final-28bbef29，UI83=82PASS/1FAIL(shots-trace)，coverage40PASS。两套源/head前后0，均已结束无活浏览器。失败原报告：
`.pi-orchestrator/v4/teams/frontend/screenshots/f3/runs/final-28bbef29-cbf1-48f6-9227-1aa83486c330/explicit-mock/shots-trace/0e73b4ac-dd10-4c46-912e-a3d53c2b5ad2/report.json`。

最后document是zh-CN，不是只能ES。实际同document Trace200/7tasks，132.4ms response解析返回；138ms捕获TypeError `Cannot read properties of undefined (reading 'taskChain')`，render→load。catch清content，5秒后anchor0/busyfalse；前三zh-CN Trace文档均7rows正常。

现图库是defer，随后普通inline立即load；ready()只ensureScope。图库末尾赋值 `window.AnotifyGraphs`（不是Anotify.graphs），读取taskChain时它尚未存在。静态base注释本来写Trace等待DOMContentLoaded，与实际立即启动不一致。此新中文现场证明缺少启动依赖屏障；旧四个ES是否完全同原因尚未证，不能倒写旧报告说都已复现。

## 单writer最小生产授权

只 `web-src/pages/trace.html` 的初始load／必要同load依赖检查：
- 根据document.readyState/DOMContentLoaded一次事件使defer脚本执行完成后才首次调用render，不能只等namespace-ready就访问图库。
- 处理页面已interactive/complete与初始loading两种路径，不错过event／挂死；既有重试调用必须遵相同依赖准备和明确failure分支。
- 明确检查 `window.AnotifyGraphs.taskChain` 实际能力；加载失败/图库缺失仍显示真实错误与原重试，不静默删图或假成功。
- 保静态script顺序/CSP，不动态eval、复旧API、循环poll/setTimeout等待或新增全局ready协议。Scope/HTTP/body/model/TaskUID/导航/CAS全部不改。
- 仅Trace依赖屏障，禁止base/partials/graphs/model/locale/API/Go大改；可更正base相关注释但无行为变动。

## 必须有真实确定性RED/GREEN

用独立新case/diagnostic ID延迟graphs.js而快速TraceAPI，原代码必须触同taskChain缺失失败，修后7Task rows/真实图和canonical node click正常；不扩大原2200/5s等待阈值。

graphscript实际失败／缺失同样必须明确可见error且无fake成功。原重试仍存在；脚本之后恢复可用时点击实际retry应恢复API/图（不自动另拉来源、不改超时或全局failure白名单）。普通已有loaded-page路径仍成功。

新增回归在scripts/v4-frontend.test.mjs，专用helper若真实被运行则f3-prefix或精确纳epoch。旧Node199不删、195完整注册组及304矩阵不减。所有原21、旧4、此次新1失败记录保，不重占旧run；生产修改前后与真实schema输入不变。

原5stage门禁：makefe、sitegencount1、Node199+新增、Vocabularycount1、累计6364diffcheck。原生病例有界/真实exit/ownedPGcleanup。fix后fresh仅这一依赖规则门槛，新source/artifact/uniqueREADY完整五suite各一次；真正CoreNode五API/cross仍待，不用本局部修复冒全v4完成。
