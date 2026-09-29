#!/usr/bin/env node
'use strict';
// Rewires MagicPush's server-side log directories to process.env.LOG_DIR,
// which static/start.sh points at the package log/ dir. Upstream hardcodes
// app/server/logs for both winston transports and the mkdir in app.js; in
// the UGOS native sandbox the app/ tree is a read-only view (only
// data/log/cache are writable), so mkdirSync there kills the process at
// module load with ENOENT. Kept as a build-time patch instead of a fork:
// every replacement below exits 1 if the pattern is missing, so an
// upstream refactor fails the build instead of shipping a package that
// cannot start.

const fs = require('fs');
const path = require('path');

const serverDir = process.argv[2];
if (!serverDir) {
  console.error('usage: patch-log-dir.js <path-to-server-dir>');
  process.exit(1);
}

function patch(relFile, from, to) {
  const file = path.join(serverDir, relFile);
  let src = fs.readFileSync(file, 'utf8');
  if (!src.includes(from)) {
    console.error(`[patch-log-dir] pattern not found in ${relFile}:\n${from}`);
    process.exit(1);
  }
  src = src.split(from).join(to);
  fs.writeFileSync(file, src);
  console.error(`[patch-log-dir] patched ${relFile}`);
}

patch(
  path.join('src', 'utils', 'logger.js'),
  "const path = require('path');",
  "const path = require('path');\n\n" +
    '// UGOS 原生沙箱里 app/ 只读，文件日志目录由 start.sh 注入的 LOG_DIR 指到可写目录\n' +
    "const LOG_DIR = process.env.LOG_DIR || path.join(__dirname, '../../logs');"
);

patch(
  path.join('src', 'utils', 'logger.js'),
  "path.join(__dirname, '../../logs/error.log')",
  "path.join(LOG_DIR, 'error.log')"
);

patch(
  path.join('src', 'utils', 'logger.js'),
  "path.join(__dirname, '../../logs/combined.log')",
  "path.join(LOG_DIR, 'combined.log')"
);

patch(
  path.join('src', 'app.js'),
  'const logsDir = path.join(__dirname, \'../logs\');',
  "const logsDir = process.env.LOG_DIR || path.join(__dirname, '../logs');"
);

console.error('[patch-log-dir] done');
