# vendor

本地第三方库。**不引 npm 构建**（项目是纯静态站点），所以库以单文件放这里，
页面直接 `<script src="/vendor/xxx.min.js" defer>`。

构建期 `scripts/hash.mjs` 会给这些文件加内容指纹（`xxx.min.<hash>.js`），
所以 HTML 里写不带哈希的路径，产物里会自动改写成带哈希的。

## 清单

| 文件 | 版本 | 许可证 | 用途 | 来源 |
|---|---|---|---|---|
| `marked.min.js` | 4.x | MIT | 渲染 Markdown（index.html 的消息正文） | npm `marked` |
| `cytoscape.min.js` | 3.34.4 | MIT | 图渲染（源的下游流向图 #88） | npm `cytoscape` |
| `cytoscape-dagre.min.js` | 4.0.0 | MIT | Cytoscape 的 dagre 分层布局（有向图自动排版） | npm `cytoscape-dagre` |

## 为什么不能依赖 CDN

e2e 的离线门禁（`scripts/e2e/lib/harness.mjs` 的 `mockExternalCdn`）会拦截
所有外网请求。用 CDN 的页面在门禁里直接挂，而且内网/离线部署也用不了。

## 更新方式

```bash
cd /tmp && npm pack cytoscape@3.34.4
tar xzf cytoscape-3.34.4.tgz
cp package/dist/cytoscape.min.js /path/to/anotify/web/vendor/
```

更新后要同步改本表的版本号，并跑 `make fe` 重新指纹。
