#!/usr/bin/env node
// Registered standalone i18n_coverage: complete 19 pages x four languages + retained behavior.
// E2E_MOCK_ONLY=1 explicitly opts into preview; default real never falls back.
// Named AC/v3 retirement inventory: team F3-TEST-MAP.md (F3b2a appendix).
import { fileURLToPath } from 'node:url';
import { localeMain } from '../fixtures/f3/f3-i18n-runner.mjs';
await localeMain('i18n_coverage', fileURLToPath(import.meta.url));
