# 三项几何红测：已有边界的准确断言（父批准）

父已读取实际PARENT-REPAIR-logs/geometry-summary.json。旧full21失败与这次诊断原始红保留；不是为了绿修改生产布局。

## 预布局：比较同一视觉边界，不把内盒当外盒

实际expanded：pre #page-main.x264/padding32、内.page.x296；mounted outer main.x264、#page-main.x296/.page.x296。
实际collapsed：pre #page-main.x76/padding32、内.page.x158；mounted outer.x76、content.x108、因maxWidth居中的.page.x158。158不是漂移，premount与mounted视觉内容坐标相同。

批准仅测试两prelayout函数：
- 原始premount #page-main是外边界，必须正确预留sidebar宽；挂载后同一外边界是outer main，必须等于sidebar.right。不能强制mounted的内层#page-main.x等于sidebar.width。
- 同一document从premount至mounted的实际.page视觉x保持一致，内容gutter使用实际computed padding/容器关系，不加硬编码32、不删除collapsed保护或允许漂移。
- 保所有早帧、DOM/paint/mount身份和旧数据；不用把测量推迟到字体/数据/接口完全稳定后，不能以“元素还没准备好”跳过已存在的premount内容。
- 正向与定点负变异都要判别：破坏预留sidebar offset或挂载outer边界/挪动.page，测试必须红；不是只验证这次恰好296/158数字。

## Namespace切换：新文档构建与已挂载槽位区分

实际旧document slot53/navY117；native切换触发新document，旧断言执行71.8ms时整个body/main/sidebar/slot/nav尚未存在；首次paint108ms，首次mountedRAF140.6ms已经slot53/nav117，prod请求175.9ms时相同。未发现任何已挂载slot0帧。

批准仅namespace交互测试：
- 跟踪document identity和所有early样本。新文档sidebar尚未挂载时保留absent事实，不把它伪造为mounted slot=0；不能连原始数据也删除。
- **从第一帧sidebar/同步shell已挂载起**，scope-slot必须存在且满足原>=40高度，不能只等很晚恢复后测。已挂载sidebar却slot缺失、0高度或后续丢槽仍必须FAIL。
- 仍需真实unique native prod选择→URL/storage→当前document发prod请求，保旧/新namespace身份、slot/nav位置与实际数据。有界等待仅用于取得新document的第一次mount/必要请求，不等所有异步接口稳定抹掉中间坏帧。
- 补负变异证明mounted slot删除/高度0立即红，即使后续恢复；原new-draft/CAS/nav/sidebar其它case不削。

## 所有权和执行

父已授权的同一唯一FE repair writer可追加 `f3-ui-cases.mjs` 仅这3函数及必要现测试诊断helper/Node负向回归，生产partials/CSS/template不改。原F1/F2/raw五叶已绿不重做。

当前stage原5机械verify+新诊断ID的3真实正向几何/定点负向测试实际执行，保原FAIL。新全5只能父在新源码/生成产物/fresh门槛和唯一READY完成后运行，不重占final-08af；原195/304/180测试与真实跨队门槛保持。
