# i18n Key 命名规范（S2/S3 必须严格遵守，避免 key 不一致返工）

## 结构（YAML 嵌套 → 扁平 key，模板用 {{t "page.key"}}）
- 命名空间 = 页面：`index.*` `login.*` `receivers.*` `keys.*` `security.*` `docs.*`
- 共享文案：`common.*`（导航 nav、页脚 footer、按钮 button、通用词）
- 层级用点分：`index.kpi.total` `common.nav.overview`

## 各页必备 key（S2 在页面用 {{t}} 引用，S3 在 locales 填值；两边 key 必须一致）

### common.*（共享，partials.js 导航/页脚 + 通用）
- common.nav.overview=总览  common.nav.receivers=通知接收  common.nav.keys=API Keys
- common.nav.security=安全与登录  common.nav.api=接入文档  common.nav.logout=退出登录
- common.footer.copyright=页脚版权行
- common.brand.tagline=Agent 完成即通知

### index.*
- index.title=总览  index.subtitle=你的通知一览  (mountLayout 用)
- index.greeting=你好  index.loading=正在加载你的通知统计…
- index.kpi.* / index.heatmap.* / index.quickstart.* / index.recent.* / index.detail.*

### login.*
- login.title=登录  login.welcome=欢迎回到 Anotify  login.subtitle=无需密码…
- login.tab.login=登录  login.tab.register=注册  login.button.*=…  login.footer=…

### receivers.* / keys.* / security.* / docs.*
- <page>.title / <page>.subtitle（mountLayout）+ 各页正文 key

## 规则
1. S2 负责在 pages/*.html 里用 {{t "..."}} 替换所有静态中文文本，key 按上表命名
2. S3 负责在 locales/zh-CN.yaml 和 en.yaml 填这些 key 的值，并生成 partials.js 的 i18n 读取
3. mountLayout 的 title/subtitle 仍是 JS 传入 → 走 i18n.js（S3 把 partials.js 的 NAV/footer 改为读 Anotify.t）
4. 缺 key 时 t 函数回退中文（sitegen 已实现），JS 侧 Anotify.t 缺 key 回退传入默认串
5. 英文翻译要地道（技术产品语气），不是机翻
