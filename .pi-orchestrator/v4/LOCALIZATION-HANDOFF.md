# UI 翻译与原始诊断内容边界（父 L10n 裁决）

## 必须翻译

导航、按钮、列名、标签、提示、确认框、空/错/加载状态等**应用固有 UI chrome**必须四语言一致。已知机器reason可映射本地化解释，但不能改动原始事实。

## 保留原文

用户输入的名称/描述/载荷、脚本输出、代码片段、服务端原始诊断message等原始内容保持原文；EN界面可以出现其中文内容。不得为通过零CJK检查翻译、清空、替换或截掉这些字段。已批准凭证/敏感字段脱敏仍必须执行，raw内容标记不是免安全检查。

- 明确的动态原文**叶节点**可用 `data-raw-content` 标记，例如原始日志文本或API message的独立span。周边说明、折叠按钮、错误标题仍翻译。
- 不标整个card/dialog/table/pre/trace界面去笼统豁免；一段原文标记不能覆盖其中混入的应用固有中文标签。
- Scope trigger/menu 中真实 Namespace `metadata.displayName`／name（或已验证的显式选中 namespace 名字）是原始资源数据，保留后端默认中文名也不翻译。仅最内层实际数据文本SPAN可标记；BUTTON/整个control/menu、前端本地化前缀与加载/选择/空态提示不得标记。数据渲染转回loading/提示时移除marker，不能让旧标记豁免后来UI文案；HTML-like名字仍纯文本安全呈现。
- 检查器对原文叶做有限排除后核UI chrome，同时用唯一原文sentinel断言原文确实保留；向chrome故意注入中文仍要让测试红。
- 原文中的网络/缺接口错误仍属于真实失败，不能因为它被标raw就让功能用例绿。比如mock缺PATCH的501，仍需检测请求状态/错误与实际持久值，不能只用i18n豁免躲过。
- 服务端自由文本不作为新机器协议做全局substring翻译；可用局部译后的上下文加原始详情，保留已有身份域 `{error}` shape。
- shared `loadErrorNode` 保留本地化状态标题/操作按钮，并可在实际错误容器中追加独立、可见的 `data-raw-content` message 叶。只呈现上游已解码的真实 `err.message` 字符串，以 `textContent` 或严格escape渲染，不能反射整错误对象、请求参数、URL/header/stack或HTML。401导航流程不变，网络/5xx仍是失败；原文标记不等于功能测试成功。浏览器应定位实际错误容器的该可见叶，而非假定一切错误都出现在toast。

## JA Device 具体 DTO（已核现 Go）

`PATCH /v1/devices/{id}` 属保留独立设备域，不套Resource信封/CAS。关停body精确 `{ "enabled": false }`；还支持name/eventScope/tags，省略或null保原，tags:[]清空。

- 成功200：`{ "ok": true, "device": <Device camelCase object> }`，enabled=false必须保留。
- 当前会话用户ownership；他人或不存在404，匿名401 `{error:"未登录"}`。不是新Node授权或metadata.resourceVersion。
- handler `internal/server/handlers.go:107–166` 调UpdateDevice；Device JSON字段 `internal/store/devices.go:11–25`；会话 `internal/auth/session.go:188–197`。

Mock缺handler导致501，应按以上精准镜像和持久GET验证修；已正确的receivers UI乐观render不因fixture错误重写，失败回滚/重新GET与用户可见错误仍保留。
