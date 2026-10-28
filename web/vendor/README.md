# vendor

本地第三方库。**不引 npm 构建**（项目是纯静态站点），所以库以单文件放这里，
页面直接 `<script src="/vendor/xxx.min.js">`。

构建期 `scripts/hash.mjs` 会给这些文件加内容指纹（`xxx.min.<hash>.js`），
所以 HTML 里写不带哈希的路径，产物里会自动改写成带哈希的。

## 清单

| 文件 | 版本 | 许可证 | 用途 | 来源 |
|---|---|---|---|---|
| `marked.min.js` | 4.x | MIT | 渲染 Markdown（index.html 的消息正文） | npm `marked` |
| `dagre.min.js` | 3.1.3 | MIT | **只算图布局坐标**（分层 + 排序 + 减少交叉） | npm `@dagrejs/dagre` |

配套的自研渲染器在 `web/flograph.js`（不是 vendor：它是我们自己的代码）。

## 为什么不用图库（Cytoscape / G6 / vis）

一开始用的是 Cytoscape.js，但它的节点本质是「一个背景色 + 一条边框 + 一段
文本」—— 只能画图论网络图，画不出 n8n 那种卡片节点（图标 / 标题 / 副标题 /
状态点 / 悬浮操作），也没法给单个能力自定义外观。

所以改成：**dagre 只算坐标，节点用 HTML 画，连线用 SVG 画**。
节点是真正的 DOM，样式完全自由（`web/flograph.js` + `ui.css` 的 `.wf-node`）。

代价是连线路由和缩放平移要自己写（约 400 行），换来的是节点可以任意设计。

## 为什么动画也不引库（anime.js / GSAP / Motion One）

链路图的入场动画只有两类动作：节点淡入 + 连线沿路径"长"出来。
后者在业内**统一**是 `stroke-dasharray` + `stroke-dashoffset` 的做法
（anime.js / GSAP / Motion One 画线也是这一招，只是包了层 API）。

所以直接用了浏览器原生的 **Web Animations API**（`element.animate()`）：
零依赖、不用 vendor、不用构建，而且拿到 `onfinish` 回调。

**别再手写 CSS transition 做这个**：先设 `transition` 再设初值，初值
那一跳会被过渡自己吃掉，浏览器最终只看到 `0 → 0`，线会"啪"一下整条
出现（踩过两次）。判据见 E2E「线是逐段画出来的（不是整条瞬现）」——
它盯的是 `stroke-dashoffset` 的中间值，只看"动画在播"是发现不了的。

## 为什么不能依赖 CDN

e2e 的离线门禁（`scripts/e2e/lib/harness.mjs` 的 `mockExternalCdn`）会拦截
所有外网请求。用 CDN 的页面在门禁里直接挂，而且内网/离线部署也用不了。

## 更新方式

```bash
cd /tmp && npm pack @dagrejs/dagre@3.1.3
tar xzf dagrejs-dagre-3.1.3.tgz
cp package/dist/dagre.min.js /path/to/anotify/web/vendor/
```

更新后要同步改本表的版本号，并跑 `make fe` 重新指纹。
