#!/usr/bin/env node
// npm installs only the platform package that matches this machine (its os and cpu fields).
'use strict';
const { spawnSync } = require('node:child_process');
const path = require('node:path');

const pkg = `intact-proxy-${process.platform}-${process.arch}`;
const exe = process.platform === 'win32' ? 'intact.exe' : 'intact';
let binary;
try {
  binary = path.join(path.dirname(require.resolve(`${pkg}/package.json`)), 'bin', exe);
} catch {
  console.error(`intact: no binary for ${process.platform}-${process.arch}. The package ${pkg} is not installed.`);
  console.error('Install again without --omit=optional, or build from source: https://github.com/louisphamdev/intact');
  process.exit(1);
}
const r = spawnSync(binary, process.argv.slice(2), { stdio: 'inherit' });
if (r.error) {
  console.error(`intact: ${r.error.message}`);
  process.exit(1);
}
if (r.signal) process.kill(process.pid, r.signal);
process.exit(r.status ?? 1);
