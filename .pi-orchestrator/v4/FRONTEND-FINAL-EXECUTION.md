# 前端固定版本完整五套件 · 父机械门槛

## 冻结与已核前提

- SourceVerificationHEAD `ca302bcef70dae15b9479311dd70950efb08969f`；clean DeliveryHEAD `6364b92b3e21807b93fb6e72a2c561147912c97c`。
- 前端完整WT：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-frontend-delivery`。原manager已父safe stop，全部writer/review/命令此前已收，无后续源码写入；不删除WT/session。
- FULL source/artifact SHA `7180e345f9b87c9366508245e048094169563bfbf27a03502ba3b38764ff2b40`；父实际调用现captureEpoch只读重算匹配。
- 唯一完整run `final-08af0312-ac57-40a5-ac7e-c50b6f998125`；195个注册ID分35/29/40/83/8，父逐suite核无重复，各slot此前未执行。
- 旧902 READY已withdrawn；旧worker缺MOCKONLY导致rejected与新正确env的scoped11组/180tests/49sitegen、finite3review保留分开。**没有把旧rejected改成verified。**

## 执行方式

父read-only mechanical carrier workers，不自行重复执行suite：实际命令只由launch verify数组执行，所有case各一次。脚本 `/tmp/anotify-v4-fe-final-gates/run-suite.sh <suite>` 已保存；任务默认rootstore持记录、只读前端WT，不创建实现子任务。

该脚本明确cd上述WT，固定HEAD及READY状态/sourcebeforeafter检查，设E2E_MOCK_ONLY=1、F3_RUN_ID、F3_SOURCE_HASH、F3_CONCURRENCY=2；每suite完整入口外timeout1200+10秒KILL，全tee至READY的fullLog，保存真实command/epoch/head退出码到 `/tmp/anotify-v4-fe-final-gates/<suite>-status.json`。原CLI内部完整各case/一次manifest/聚合与cleanup不绕过。既有日志存在即拒绝二次运行，不retrylatest绿。

## 当前调度（不是PASS）

首批受管gate：
- `v4-fe-full-frontend` → `frontend`35组。
- `v4-fe-full-ui` → `ui_shift`83组，包含304矩阵及其他视图。

后续待派（完整数量不减少）：
- i18n 29组。
- i18n_coverage 40组。
- ux_regression_19_23 8组。

主机8CPU/16GB约9GB可用；最多同时2个suite（每suite2case，合计最多4），避免无限10浏览器并发影响真实几何/超时。首批完成空出槽后按同一冻结版本派余3，不能源码修改后混合聚合。

首批frontend已真实完整执行35：33PASS/2FAIL，commandExit=1而前后source/head检查=0；原日志与 `/tmp/anotify-v4-fe-final-gates/frontend-status.json` 保留，机械任务rejected。只读事实核：observe-errors故障跨goto被initScript清空，实际请求200所以正常图五按钮被当retry；playbook-delete后helper仍取已删父包的installs分组列表触404，不等于installed资源或单回执被删。未修改源码、未重跑任何ID；后待所有同epoch套件收齐再精准裁决修复，不能放宽断言/让deleted包继续200。

frontend完成后派的i18n已完整结束20PASS/9FAIL（76page/lang实际尝试65PASS/11FAIL）；UI已05:02完整结束80PASS/3FAIL、83组无unrun，304矩阵实际PASS但另外3几何检查未过，350/353 view实际PASS。两套commandExit1且source/head检查均0，不能因矩阵通过称UI全绿。

05:08处理UI L1 silence提醒时实际gate已retired/日志聚合与statusJSON完成，没有活verifier/browser；不abort/重启或重复执行。UI失败为expanded/collapsed预布局main.x与sidebar.x差32、namespace切换slot高度；i18n有真实数据/原诊断CJK被按chrome等未裁缺口，父先保完整事实，不就此放宽断言。

最后两套也已完整收齐：i18n_coverage 33PASS/7FAIL，UX 8PASS/0FAIL且真实工具accepted(verified)。合计**195组全部执行一次，174PASS/21FAIL**，五个真实commandExit与汇总一致，source前后及HEAD检查全0。完整机器汇总 `/tmp/anotify-v4-fe-final-gates/full-five-summary.json`。

完整冻结基线结束后父限域只读诊断：16重复locale组归五真实数据producer叶，不是固有UI词漏译；expanded/collapsed预布局量mounted内层盒而outer盒尚未取证；namespace-switch跨reload时slot未记录box。父新唯一v4-fe-parent-gate-repair接safe旧WT，只五数据叶+fault导航/单回执helper及几何被动证据，不改未获证的layout/CSS/partials。原195/304和180Node保，旧final-08af不再执行。新源码全部缺口清后再新READY/各suite一次，不用部分绿或UX通过覆盖四套失败。

任一失败保完整原始记录，不能重新执行同run/ID或挑绿case；待父具体根因裁决后新源码/版本另启动完整gate。worker ready JSON只表示前提交机械verifier，最终是否通过以extension实际command exit和完整聚合/源码一致为准；不自报测试已执行或用空条件升级。

## 修复后第二次完整版本（当前）

- 源码冻结 `273b0cb858ea424baecdbe4dd15795be6b603b70`；父实际重跑makefe/sitegen49/Node **195/195**（原180保）/Vocabulary/累计6364 diffcheck全部0。Node test195与完整suite group195是两个独立计数，不混为一个结果。
- raw五叶/fixture故障/删除后单回执/真实geometry正负反例已限定fresh门槛通过；最后补两runtime/testhelper descriptor与parentgroup404 coverage，lossless14空行原stdout验证也fresh通过，无已知阻断finding。
- 新ignored READY：该WT `teams/frontend/PARENT-FINAL-READY.log`（完整前缀`.pi-orchestrator/v4/`）。SourceVerificationHEAD/DeliveryHEAD均273；CodeSHA `9945a14fd04c4f56cf6f3dd55fdf92efe62d82fc65963b9f5002e8e28eaf04c7`，FULLSHA `a184caa4ac94aae696d88546b6b1fa4253a22f92091e65fb097e6025a91c097c`。
- 新完整run：`final-aa38ef83-fd7a-4867-a210-6b6ecbc0e265`。旧final-08af/17421完整证据不改，不重占case。
- 父`/tmp/anotify-v4-fe-final-gates/run-suite-post-repair.sh`固定新HEAD/READY/hash，每suite1200有限/fulltee/源码前后校验/退出JSON。首批 `v4-fe-final2-frontend`35与`v4-fe-final2-i18n`29已派，最多4case；后coverage40/UI83/UX8按空槽同epoch各一次继续。
- 首两套实际完成且工具accepted(verified)：frontend **35/35**、i18n **29/29**（76page/lang全部通过），原epoch/head前后均0。
- 第二批已派 `v4-fe-final2-ui`83与`v4-fe-final2-coverage`40；最后`v4-fe-final2-ux`8也已派，verify先用/tmp中的有界fs.watch等待当前两套一个status完成，再执行UX完整一次。最多两suite/4case，避免通知延迟让最后任务长时间未派；事件等待900秒，无忙循环/浏览器。任何源epoch变动拒绝继续。
- 第二次全五套件也已实际完整结束：frontend35/0、i18n29/0、coverage39/1、UI80/3、UX8/0，总 **191PASS/4FAIL**，五源/head前后检查0。完整汇总 `/tmp/anotify-v4-fe-final-gates/post-repair-full-five-summary.json`；此次所有几何修正与原数据叶case通过，剩4均ES Trace成功锚点缺失（UI三主题视口及coverage pages-es-5），不算已全部通过。
- 07:10处理UI07:00 L1 silence时实际UI已07:01结束，日志/status/聚合齐全、无活verifier/browser；不abort/重启/复用case。当前304矩阵全部实际尝试，3ES Trace检查未过，353views尝试350通过，不用actualMatrix=304冒称矩阵全部PASS。
- 父两限域scout核四失败都TraceGET200/kindTrace但无response body/trace-error/加载渲染时间记录；代码catch可能清content并撤busy所以pageerror空不证明无异常。真实根因未证，不能直接加timeout或改TaskName/UIDfallback。
- 新唯一 `v4-fe-trace-diag` 仅测试观察/专用诊断：捕获同次响应公共ID/字段类型、实际错误容器及渲染时序，ES链与English/ESdarkmobilePASS对照；不改production或运行全套到绿。旧final-aa38/1914记录保，Source发生下一改动后另epoch再完整各一次。

## 真实失败归档与一次诊断重演（不是产品通过）

旧full2仍191/4失败、根因未知。独立Trace诊断0611be5三次未复现，不能代替旧功能门槛。新4db5b35646402a01cf14e2ec3998258eabfb1ad5只test observer：同documentId记录Trace安全响应shape/publicIDs及真实caught error、busy/content/anchor/mount时间。synthetic原生render错误准确触发原anchorFAIL/exit1且归档成功；正常对照7行，Node199/原195保。自由错误字符串脱敏存在未标识秘密的理论边界，仅受控mock诊断，不作为生产原文日志安全保证。

父已固定makefe新观察epoch并生成ignored PARENT-TRACE-REPLAY-READY.log：FULLSHA `b2a2f67b1122bfb232ed6e0bcfd57e4b63f4d9821492502f573b29a7b3c6ecb5`，唯一新run `final-28bbef29-cbf1-48f6-9227-1aa83486c330`。

- `v4-fe-trace-replay-ui`→原UI83完整断言一次。
- `v4-fe-trace-replay-coverage`→原coverage40完整断言一次。

实际verify脚本 `/tmp/anotify-v4-fe-trace-replay/run-suite.sh`，每suite1200限制、并发2/总4case、fulltee/oncemanifest/source前后检查。保存purpose=TRACE_DIAGNOSTIC_NOT_PRODUCT_ACCEPTANCE。不是第三次最终全5，也不冒未执行其它72组；一次观察若PASS仍保旧rootunknown，不重试旧ID或洗失败。若再次FAIL用新sameDoc现场裁原因后才精确修生产/fixture；原timeout/assertions不变。

## 最终仍须确认

195 groups完整记录/all actualview证据、缺/失败/重复非零、源/产物epoch一致、所有进程回收。原makefe/sitegen/Node三项最终机械close按当前源码独立收齐，不能由180self报告顶替。真实T1五spec/backend/Node/前端跨队CI/build/fullE2E和页面仍最终门禁，隔离MOCKONLY不是默认服务fallback、也不表示v4整体完成或已合父分支。
