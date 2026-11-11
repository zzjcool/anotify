#!/usr/bin/env node
// Registered standalone UX19–23: member admin denial, notification denied, /auth/me identity,
// empty passkey enrollment, zero-device honest denominator, plus retained device PATCH,
// guest route guards and the exact-password login form.
// Explicit preview only under E2E_MOCK_ONLY=1; the default authority is the real server.
import { fileURLToPath } from 'node:url';
import { uiMain } from '../fixtures/f3/f3-ui-runner.mjs';
await uiMain('ux_regression_19_23', fileURLToPath(import.meta.url));
