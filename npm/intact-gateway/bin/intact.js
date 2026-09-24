#!/usr/bin/env node
// npm installs only the platform package that matches this machine (its os and cpu fields).
'use strict';
const { spawnSync } = require('node:child_process');
const path = require('node:path');

// Platform packages share this package's name, scope included (npm or GitHub Packages).
// npm refused the name intact-proxy-win32-x64 as spam, so Windows x64 keeps a separate name.
const self = require('../package.json').name;
const target = `${process.platform}-${process.arch}`;
const pkg = `${self}-${target === 'win32-x64' ? 'windows-x64' : target}`;
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
