# 侧栏管理入口的已证布局位移：精确修复授权

E1b被动证据98帧记录同一nav/shell：fonts ready、header/scope/hint均未改变；auth/me完成后admin section去hidden并填充，80.31px+已有12px间距推main nav.y由162到254.31。这是初次同步mount/首paint之后的真实位移，不是字体/banner或测试不同节点。

## 最小修改

固定049c992的web/partials.js:1308–1331，flex兄弟顺序为brand→scopeSlot→adminNav→nav→footer。批准**仅1324–1325的append顺序**改为brand→scopeSlot→nav→adminNav→footer。

- 移动现有admin section，不重建、克隆或删除节点；role判定、href、active/aria-current、事件绑定及顶部独立入口原样。
- loadMe角色门禁不改：匿名/member不能看到管理入口，admin收到身份后仍能访问；不靠在首帧展示未授权入口避免移动。
- main nav顶边与已有链接位置不受late admin填充影响。nav作为flex剩余区域的高度可自然改变，既有case只强断nav.y，不需要删/放宽断言。
- 不预留硬编码空白、不更改CSS/语言hint/字体或推迟测量到接口稳定后；不直接修改已通过的G1图、G2mask、L1身份流程。

## 顺序与验收

当前E1b唯一writer先安全收齐。下一单writer限定partials两append、相应最小Node测试/原shift-lang-hint诊断用例与截图证据；不与F4/helper产物写入并发。

父另允许同一S1 writer精准补 Scope trigger/menu 的两处原始Namespace数据最内层SPAN标记，规则见LOCALIZATION-HANDOFF；不得把BUTTON/UI前缀/加载提示泛标为raw，不改数据、范围或角色业务。

原4机械门禁+原nav强位置断言的native>=60frames/四视口，再验证admin/member/guest可见权限与管理链接原生可用、侧栏主链接/平移/滚动/尾部不溢出。保原98帧红证据和修后绿，不以等待更多帧/忽略早帧/硬像素豁免放绿。

修改后重新makefe并更新F4最终source+generated artifact epoch；旧E1b READY仍为历史，不用于最终五套件结果聚合。精准fresh复审无major后，完整五suite与父真实跨队门禁仍不可省。
