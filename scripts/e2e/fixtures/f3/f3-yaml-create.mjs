// W4 · 「从 YAML 创建」两条端点（yaml-create 契约 §6）的浏览器行为用例。
//
// 被测的两条写端点（operationId 的名字由 `make contract` 对账）：
//   POST /v1/namespaces/{ns}/capabilities/yaml   createCapabilityFromYAML
//   POST /v1/namespaces/{ns}/sources/yaml        createSourceFromYAML
// 请求体是 `{"yaml": "<整份文档>"}`（JSON 里装一份 YAML 正文，不是 application/yaml）。
//
// 这条用例走的是**真实页面入口**（列表页按钮 → 弹层 → 提交 → 列表刷新），
// 断言只落在这几件产品事实上：请求只带一个键、成功落成列表行、失败保留草稿并
// 显示带行号的原因、空正文前端直接拦截（零写请求）、凭据只写不读。
//
// 凭据语义（与既有 GET 逐字一致，别按直觉断言）：`spec.config.token` / `secret`
// 是只写字段 —— 响应里没有这两个**键**；但 webhook 源的 `spec.config.hookUrl`
// 按设计内嵌真实 token（`/v1/hooks/<token>`，控制台就是让用户复制这个 URL）。
// 所以「token 的值不出现」是错的断言，只有 secret 的值必须一个字符都不出现。
import assert from 'node:assert/strict';

const CAP_NAME = 'yaml-made';
const SRC_NAME = 'yaml-hook';
const HOOK_TOKEN = 'tok-e2e-yaml-1';
const HOOK_SECRET = 'sec-e2e-yaml-1';

const CAP_YAML_PATH = '/v1/namespaces/default/capabilities/yaml';
const SRC_YAML_PATH = '/v1/namespaces/default/sources/yaml';

// 一份完整 Capability 文档：入口脚本用 `|` 块标量写多行（YAML → JSON 这条链上
// 最容易丢换行的一段），params 用行内映射。入口带 shebang：没有它真实服务回
// entrypoint_no_shebang，那是入口校验、不是本用例的对象。
const CAP_YAML = [
  'apiVersion: anotify/v1',
  'kind: Capability',
  'metadata:',
  '  name: ' + CAP_NAME,
  'spec:',
  '  params: {}',
  '  run:',
  '    entrypoint: main.sh',
  '    files:',
  '      main.sh: |',
  '        #!/bin/sh',
  '        echo "made from yaml"',
  '',
].join('\n');

// 同样一份文档，但第二行少一个冒号（`kind Capability`）：唯一的语法错，
// yaml.v3 报 `could not find expected ':'` 定位到那一行 → 服务端把它翻成带行号的消息。
const BROKEN_YAML = [
  'apiVersion: anotify/v1',
  'kind Capability',
  'metadata:',
  '  name: yaml-broken',
  'spec:',
  '  run:',
  '    entrypoint: main.sh',
  '    files:',
  '      main.sh: |',
  '        #!/bin/sh',
  '',
].join('\n');

// 一份 webhook 源文档：凭据写在 config 里（只写字段），targets 指向上面那个能力。
const SRC_YAML = [
  'apiVersion: anotify/v1',
  'kind: Source',
  'metadata:',
  '  name: ' + SRC_NAME,
  'spec:',
  '  kind: webhook',
  '  enabled: true',
  '  config:',
  '    token: "' + HOOK_TOKEN + '"',
  '    secret: "' + HOOK_SECRET + '"',
  '  targets:',
  '    - capability: ' + CAP_NAME,
  '',
].join('\n');

export async function yamlCreate(d) {
  const { page, check, shot } = d;
  const visible = (sel, timeout = 15000) => page.locator(sel).waitFor({ state: 'visible', timeout });
  const hidden = (sel, timeout = 15000) => page.locator(sel).waitFor({ state: 'hidden', timeout });
  // 弹层开/关除了 class 之外还要查可见性：`hidden` 类才是关闭态的唯一判据，
  // 光看元素存在会把「没关掉」判成通过。
  const modalOpen = async (sel) => {
    const box = page.locator(sel);
    if ((await box.count()) === 0) return false;
    return box.evaluate((n) => !n.classList.contains('hidden') && n.getBoundingClientRect().height > 0);
  };
  // 当前文档里对某条写路径的 POST（driver 在每次导航时重置调用记录）。
  const postsTo = async (url) => (await d.calls()).filter((c) => c.url === url && c.method === 'POST');
  // 等某条 POST 拿到指定状态码：`__f3Calls` 在 fetch 发起时就落一条，
  // 所以必须等 httpStatus 落下来再读，否则读到的是「还没有状态」的那条。
  const waitPost = (url, status) =>
    page.waitForFunction(
      ([path, code]) => (window.__f3Calls || []).some((c) => c.url === path && c.method === 'POST' && c.httpStatus === code),
      [url, status],
    );

  /* ---------- YAML-01 能力：粘一份完整文档就真的建出能力 ---------- */
  await d.goto('capabilities');
  await visible('#caps-yaml');
  await page.locator('#caps-yaml').click();
  await visible('#cap-yaml-modal:not(.hidden)');
  await page.locator('#cap-yaml-input').fill(CAP_YAML);
  await page.locator('#cap-yaml-submit').click();
  await waitPost(CAP_YAML_PATH, 201);
  const capPosts = await postsTo(CAP_YAML_PATH);
  assert.equal(capPosts.length, 1, '从 YAML 创建能力必须只发一次 POST …/capabilities/yaml，实际 ' + capPosts.length);
  const capPost = capPosts[0];
  assert.equal(capPost.httpStatus, 201, '从 YAML 创建能力必须 201，实际 ' + capPost.httpStatus);
  // 请求体只能有 `yaml` 一个键（契约 §1.3）：多一个键服务端就 400 unknown_field。
  assert.deepEqual(capPost.keys, ['yaml'], '写请求体只能有 yaml 一个键，实际 ' + JSON.stringify(capPost.keys));
  assert.equal(capPost.body.yaml, CAP_YAML, '贴进 textarea 的正文必须原样发出（块标量不能被重排/截断）');
  assert.equal(capPost.result && capPost.result.kind, 'Capability', '201 必须回 Capability 信封');
  assert.equal(capPost.result && capPost.result.metadata && capPost.result.metadata.name, CAP_NAME);
  // 成功：弹层关闭（不跳转）+ 列表里真的长出一行。两步分开断言：任一没做到都是缺陷。
  await hidden('#cap-yaml-modal');
  assert.equal(page.url().includes('capabilities.html'), true, '成功之后留在列表页（不自动跳转），实际 ' + page.url());
  await visible('#caps-list [data-cap="' + CAP_NAME + '"]');
  check('YAML-01 capability created from pasted YAML: POST body only {yaml}, 201, modal closes, list row appears');

  // 点这一行进详情页：名字来自落库后的真实读面，不是前端本地拼的。
  await page.locator('#caps-list [data-cap="' + CAP_NAME + '"] a[data-act="open"]').first().click();
  await page.waitForFunction(() => location.pathname.endsWith('/capability.html'));
  await visible('#cap-detail[data-cap="' + CAP_NAME + '"]');
  assert.equal((await page.locator('#cap-name').innerText()).trim(), CAP_NAME, '详情页标题必须是刚建的能力名');
  check('YAML-02 capability detail page shows the name read back from the store (not local state)');

  /* ---------- YAML-03 源：凭据只写不读，hookUrl 内嵌 token 是既有语义 ---------- */
  await d.goto('sources');
  await visible('#btn-new-yaml');
  await page.locator('#btn-new-yaml').click();
  await visible('#src-yaml-modal:not(.hidden)');
  await page.locator('#src-yaml-input').fill(SRC_YAML);
  await page.locator('#src-yaml-submit').click();
  await waitPost(SRC_YAML_PATH, 201);
  const srcPosts = await postsTo(SRC_YAML_PATH);
  assert.equal(srcPosts.length, 1, '从 YAML 创建源必须只发一次 POST …/sources/yaml，实际 ' + srcPosts.length);
  assert.equal(srcPosts[0].httpStatus, 201, '从 YAML 创建源必须 201，实际 ' + srcPosts[0].httpStatus);
  assert.deepEqual(srcPosts[0].keys, ['yaml'], '写请求体只能有 yaml 一个键，实际 ' + JSON.stringify(srcPosts[0].keys));
  assert.equal(srcPosts[0].body.yaml, SRC_YAML, '源文档必须原样发出');
  await hidden('#src-yaml-modal');
  await visible('#src-list > div[data-name="' + SRC_NAME + '"]');
  check('YAML-03 source created from pasted YAML: POST body only {yaml}, 201, modal closes, list row appears');

  // 读面凭据纪律：断言落在**响应字节**上（不是解析后的对象），三个键都不许出现。
  // `hookUrl` 内嵌 token 是设计如此，所以只要求它是那个真实 URL。
  const raw = await page.evaluate(async (url) => {
    const res = await fetch(url);
    return { status: res.status, text: await res.text() };
  }, '/v1/namespaces/default/sources/' + SRC_NAME);
  assert.equal(raw.status, 200, '刚建好的源必须可读，实际 ' + raw.status);
  assert.ok(!/"token"\s*:/.test(raw.text), '读面不得出现 token 键：' + raw.text);
  assert.ok(!/"secret"\s*:/.test(raw.text), '读面不得出现 secret 键：' + raw.text);
  assert.ok(!raw.text.includes(HOOK_SECRET), '读面不得出现 secret 的值：' + raw.text);
  assert.match(raw.text, /"secretSet":true/, 'secret 只报「设了没有」：' + raw.text);
  const got = JSON.parse(raw.text);
  assert.equal(got.spec.config.secretSet, true, 'secretSet 必须是 true');
  assert.equal(got.spec.config.hookUrl, '/v1/hooks/' + HOOK_TOKEN, 'webhook 源的 hookUrl 按既有语义内嵌 token，实际 ' + got.spec.config.hookUrl);
  check('YAML-04 credential projection: no token/secret keys and no secret value in bytes, secretSet=true, hookUrl carries the token');

  // 只读详情页：真的渲染出这个源，且正文里没有 secret 明文。
  await d.goto('source-detail', 'namespace=default&name=' + SRC_NAME);
  await visible('#src-detail[data-source="' + SRC_NAME + '"]');
  const detailText = await page.locator('body').innerText();
  assert.ok(detailText.includes(SRC_NAME), '详情页必须渲染出源名（否则下面的泄漏检查是空转）');
  assert.ok(!detailText.includes(HOOK_SECRET), '源详情页正文不得出现 secret 明文');
  check('YAML-05 source detail page renders the YAML-created source and never prints the secret');

  /* ---------- YAML-06 错 YAML：400 + 行号，弹层不关、草稿不丢 ---------- */
  await d.goto('capabilities');
  await page.locator('#caps-yaml').click();
  await visible('#cap-yaml-modal:not(.hidden)');
  await page.locator('#cap-yaml-input').fill(BROKEN_YAML);
  await page.locator('#cap-yaml-submit').click();
  await waitPost(CAP_YAML_PATH, 400);
  const brokenPosts = await postsTo(CAP_YAML_PATH);
  const brokenPost = brokenPosts[brokenPosts.length - 1];
  assert.equal(brokenPost.httpStatus, 400, '坏 YAML 必须 400，实际 ' + brokenPost.httpStatus);
  assert.equal(brokenPost.result && brokenPost.result.reason, 'invalid_yaml', '坏 YAML 的 reason 必须是 invalid_yaml');
  const causes = (brokenPost.result && brokenPost.result.details && brokenPost.result.details.causes) || [];
  assert.ok(causes.length > 0, 'YAML 层问题必须带 details.causes[]，实际 ' + JSON.stringify(brokenPost.result));
  // 行号在**两个**地方都要有：顶层 message（只看 message 的前端）与 causes[].message。
  assert.match(String((brokenPost.result && brokenPost.result.message) || ''), /第 2 行/, '顶层 message 必须带行号，实际 ' + JSON.stringify(brokenPost.result && brokenPost.result.message));
  assert.ok(
    causes.some((c) => /第 2 行/.test(String((c && c.message) || ''))),
    'causes[].message 必须带行号，实际 ' + JSON.stringify(causes),
  );
  // 前端：弹层仍开着、textarea 内容原样保留、消息里同时有真实行号与可读原因。
  assert.equal(await modalOpen('#cap-yaml-modal'), true, '失败之后弹层必须仍打开（否则草稿就丢了）');
  assert.equal(await page.locator('#cap-yaml-input').inputValue(), BROKEN_YAML, '失败之后 textarea 内容必须原样保留');
  const errText = (await page.locator('#cap-yaml-msg').innerText()).trim();
  assert.ok(errText.length > 0, '失败必须在弹层里显示原因');
  assert.match(errText, /第 2 行/, '错误消息必须给出真实行号（第二行缺冒号），实际 ' + JSON.stringify(errText));
  assert.match(errText, /invalid_yaml/, '错误消息必须给出可读原因，实际 ' + JSON.stringify(errText));
  await shot('yaml-error-line');
  check('YAML-06 broken YAML -> 400 invalid_yaml with line number, modal stays open and the draft is kept');

  /* ---------- YAML-07 空正文：前端直接拦截，零写请求 ---------- */
  const postsBefore = (await d.calls()).filter((c) => c.method !== 'GET').length;
  await page.locator('#cap-yaml-input').fill('');
  await page.locator('#cap-yaml-submit').click();
  await page.waitForFunction(() => document.querySelector('#cap-yaml-msg').textContent.includes('请先粘贴 YAML 文档'));
  const emptyText = (await page.locator('#cap-yaml-msg').innerText()).trim();
  assert.equal(emptyText.includes('第'), false, '空正文的提示必须替换掉上一条错误消息，实际 ' + JSON.stringify(emptyText));
  assert.equal(await modalOpen('#cap-yaml-modal'), true, '空正文提示之后弹层不能关');
  // 拦截是同步的：留一小段窗口，确认没有迟到的写请求。
  await page.waitForTimeout(400);
  const postsAfter = (await d.calls()).filter((c) => c.method !== 'GET');
  assert.equal(postsAfter.length, postsBefore, '空正文必须零写请求，实际新增 ' + (postsAfter.length - postsBefore) + ' 条');
  assert.equal((await postsTo(CAP_YAML_PATH)).length, brokenPosts.length, '空正文不得再发 POST …/capabilities/yaml');
  check('YAML-07 empty document is rejected by the page itself (caps.yaml.empty, modal stays open, zero write requests)');

  /* ---------- YAML-08 源同名：409 source_name_used，原因可见、弹层不关 ---------- */
  await d.goto('sources');
  await page.locator('#btn-new-yaml').click();
  await visible('#src-yaml-modal:not(.hidden)');
  await page.locator('#src-yaml-input').fill(SRC_YAML);
  await page.locator('#src-yaml-submit').click();
  await waitPost(SRC_YAML_PATH, 409);
  const dupPosts = await postsTo(SRC_YAML_PATH);
  const dupPost = dupPosts[dupPosts.length - 1];
  assert.equal(dupPost.httpStatus, 409, '同名源必须 409，实际 ' + dupPost.httpStatus);
  assert.equal(dupPost.result && dupPost.result.reason, 'source_name_used', '同名源的 reason 必须是 source_name_used');
  assert.equal(await modalOpen('#src-yaml-modal'), true, '409 之后弹层必须仍打开');
  assert.equal(await page.locator('#src-yaml-input').inputValue(), SRC_YAML, '409 之后 textarea 内容必须原样保留');
  const dupText = (await page.locator('#src-yaml-msg').innerText()).trim();
  assert.match(dupText, /source_name_used/, '409 原因必须显示在弹层里，实际 ' + JSON.stringify(dupText));
  check('YAML-08 duplicate source name -> 409 source_name_used shown in the modal, draft kept and no upsert');
}
