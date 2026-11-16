# Trace 启动修复与完整前端五套件收口（仅 explicit-mock）

## 当前交付与结论

- 前端 WT：`/root/code/anotify/.pi-subagents/runs/r-57adb51b/worktrees/v4-frontend-delivery`。
- code HEAD：`1abc042ed1b79b523d81b44bf1a3a210faabecd4`；delivery HEAD：`bf21f18e433b48d093a5d597cc26ca5aa7353700`（仅新增报告）。父分支尚未集成。
- 仅 Trace 首屏 defer 图库依赖屏障/真实能力检查/原错误重试；base只注释、新增4回归及native helper。原Node199全部保留，新203全部通过；旧195组、304矩阵及原等待阈值不减。
- 三路 fresh Trace review 全 `ok:true`、0 critical/major。minor接受并记录：0-task缺库交叉态、错误前缀归因、注释过度安全断言、native包装RED非单独运行、报告耗时/自带局部epoch表述、real-mode诊断超时清理未实测。本门槛实际使用explicit-mock，不扩大生产/其他页面范围。

## 真实验收

唯一新 run：`final-trace-e4544a6f-338c-488a-b073-0705a7d3a71f`。

- SourceCodeSHA：`ff9ab45efc7c299ef3f991f580887e67243a1f3a49cfc62548a4b5586e81a771`。
- FULL SourceArtifactSHA：`338366cf0fd22ed5b62c095eae9d0f98238cb0be069b847f59df1220ab838ed1`。
- READY：WT `.pi-orchestrator/v4/teams/frontend/PARENT-TRACE-FINAL-READY.log`，原旧READY/失败run不覆盖。
- 父独立五stage：makefe、sitegen49 count1、Node203、Vocabulary count1、累计6364 diffcheck，真实exit0，日志 `/tmp/anotify-v4-fe-trace-final/parent-stage.log`。
- 完整五suite各一次、最多2suite/4case：frontend **35/35**、i18n **29/29**、i18n_coverage **40/40**、ui_shift **83/83**、UX **8/8**，总 **195 PASS / 0 FAIL / 0 UNRUN**。所有command/head/epoch前后检查均0。
- UI真实 **353/353 views**、**304个唯一page/lang/theme/viewport组合**全成功；locale两suite各 **76/76 pages**，UX **13/13 views**。
- `trace-full-five-carrier`实际工具accepted(verified)。模型“ready/no commands executed”只说模型自身未执行；验收来源是extension真正运行父固定 `run-five.sh` 的命令和原始结果，不是文字自评。
- 父深度独立audit最终exit0：直接读取195组的case/exit/suite结果与4个真实aggregate、frontend唯一manifest/enrollments；核id/attempt/namespace-independent run/cwd/mode/sourcehash、真实exit0/无signal/无timeout、真实checks>0/errors[]、每套完全相同注册集合、全304成功矩阵tuple唯一、当前源/产物hash与clean HEAD不变。
- 父audit：`/tmp/anotify-v4-fe-trace-final/parent-audit.json` 与 `parent-audit-final.log`；原完整五suite summary：`five-suite-status.json`，master和分套full log原样保留。
- 完整命令结束后父ps检查没有此run的master/suite CLI存活，不abort/重跑已结束gate；L1 no-tool通知不代表受管verify没有在执行。

## 额外 geometry 审计的精确裁决

父早期新增 `geometry.overflow===[]` 断言失败，不是原195套件失败。两个限域scout已确认当前F3 UI冻结门槛：`f3-ui-helper.mjs:174` 检查 `documentElement.scrollWidth<=innerWidth+2`；`:173/:194` 的 `overflow` 是unbounded诊断列表，没有空数组门槛。原GEOMETRY-ASSERTION-HANDOFF只涉及prelayout/scope挂载，不新增此空列表规则。

- 304/304文档的scrollWidth==innerWidth，没有文档水平滚动宽度违规。
- 136视图的诊断列表共12304条，全部unbounded；移动端包含离屏的ASIDE#sidebar及其后代（left=-264/right=0）。index另有DIV[-11,11]，具体可见性未证。
- 父audit使用**原已冻结文档宽度门槛**，仍逐条保留diagnostic记录，未改原测试/assertion/样式、未抹去观测。不将左侧所有诊断称作视觉合规证明：可见左侧越界识别是原guard覆盖局限，未证新的产品bug；保为后续视觉验收风险，不借Trace局部修复扩大生产改动。
- 父早期schema映射错误（身份在case顶层而非report、grid是pages不是views）与额外geometry断言失败日志均保留，旧 `/tmp/anotify-v4-fe-trace-final/parent-audit.log` 不覆盖；最终结果另写 `parent-audit-final.log`。某scout的“旧日志不存在”与父实际read/ls证据不符，不据此删档/补造历史。

## 未完成的边界

这只关闭Trace规则和**完整五套前端explicit-mock门槛**，不等于真实五API、Core/Node联合、G3/G4、`make ci`、`make build`、完整`make e2e`或整体v4 DONE。新增日志L1契约尚需真实Node/protocol交付与G2/G3接线；FE日志mock current/latest语义仍按LOG-RECORDS-HANDOFF由后续专属writer校准。旧21/4/1失败档案原样保留，未重试旧run/ID。

所有worker/scout派发均未指定model/preset。父 `.pi/settings.json` 原修改保留不stage；没有push/MR/merge/rebase/reset/squash或用户数据清理。
