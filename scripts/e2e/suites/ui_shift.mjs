#!/usr/bin/env node
// Registered standalone ui_shift: 19 pages x 4 languages x light/dark x desktop1440/mobile390
// (304 matrix page views) plus the retained shift anchors and genuine interactions, and the
// #384 content-tier cases (#384 主内容不用横滚：390 / 768 / 宽档 820·1024·1440，共 15 views).
// Explicit preview only under E2E_MOCK_ONLY=1; the default authority is the real server.
// Named retirement inventory: team F3-TEST-MAP.md (F3b2b appendix).
import { fileURLToPath } from 'node:url';
import { uiMain } from '../fixtures/f3/f3-ui-runner.mjs';
await uiMain('ui_shift', fileURLToPath(import.meta.url));
