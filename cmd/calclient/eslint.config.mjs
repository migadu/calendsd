// Minimal flat config for the no-build web apps: catch what a bundler or type
// checker would otherwise catch at build time (typos, undefined globals,
// unused imports) using only eslint core rules — no plugins to install.
// Run with `npm run lint` (or `npx eslint web tests`).

const browserGlobals = Object.fromEntries([
  'window', 'document', 'location', 'history', 'navigator', 'console',
  'fetch', 'AbortController', 'Headers', 'Request', 'Response', 'URL', 'URLSearchParams',
  'sessionStorage', 'localStorage', 'crypto', 'atob', 'btoa',
  'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'queueMicrotask',
  'DOMParser', 'XMLSerializer', 'Event', 'EventTarget', 'CustomEvent',
  'Intl', 'confirm', 'prompt', 'alert', 'getComputedStyle', 'requestAnimationFrame', 'CSS',
  'Notification', 'PushManager', 'ServiceWorkerRegistration', 'TextDecoder', 'TextEncoder',
  'FileReader', 'Blob', 'File', 'FormData', 'MutationObserver', 'ResizeObserver',
].map(g => [g, 'readonly']));

const workerGlobals = Object.fromEntries([
  'self', 'caches', 'clients', 'fetch', 'console', 'crypto', 'importScripts',
  'setTimeout', 'clearTimeout', 'TextDecoder', 'TextEncoder', 'Notification',
].map(g => [g, 'readonly']));

export default [
  {
    ignores: ['web/e2e/**'],
  },
  {
    files: ['web/**/*.js', 'tests/**/*.js', '*.mjs'],
    languageOptions: {
      ecmaVersion: 2024,
      sourceType: 'module',
      globals: browserGlobals,
    },
    rules: {
      'no-undef': 'error',
      'no-unused-vars': ['error', { args: 'none', caughtErrors: 'none' }],
      'no-redeclare': 'error',
      'no-dupe-keys': 'error',
      'no-unreachable': 'error',
      'no-constant-binary-expression': 'error',
    },
  },
  {
    // Service workers run against the worker global scope, not the window,
    // and the sw.js entry points are classic scripts (importScripts).
    files: ['web/**/sw.js', 'web/shared/push-sw.js'],
    languageOptions: {
      ecmaVersion: 2024,
      sourceType: 'script',
      globals: workerGlobals,
    },
  },
  {
    // Node test files additionally see node's process global.
    files: ['tests/**/*.js'],
    languageOptions: {
      globals: { ...browserGlobals, process: 'readonly' },
    },
  },
];
