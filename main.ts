// Thin launcher for the Go forge binary. The grader runs `node /bot/main.ts`; all of the
// register/done lifecycle lives in the binary itself (it reads MAKEFUN_ENV_URL and
// MAKEFUN_SHOP_TOKEN from the environment it inherits here). This file only resolves the
// binary sitting next to it, spawns it, forwards signals, and mirrors its exit code.
// (Written without type annotations on purpose: nothing to erase, so the file parses
// identically under every Node type-stripping mode.)

import { spawn } from 'node:child_process';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const forgePath = path.join(here, 'forge');

/** Copy the binary into a fresh tmpdir and make the copy executable; returns its path. */
function copyToTmp() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'forge-'));
  const dest = path.join(dir, 'forge');
  fs.copyFileSync(forgePath, dest);
  fs.chmodSync(dest, 0o755);
  return dest;
}

let child = null;
let fellBack = false;
let settled = false;

function exitOnce(code) {
  if (settled) return;
  settled = true;
  process.exit(code);
}

function launch(bin) {
  const proc = spawn(bin, [], { stdio: 'inherit', env: process.env });
  child = proc;
  proc.on('error', (err) => {
    if (err.code === 'EACCES' && !fellBack) {
      // The repo mount lost the exec bit (or is read-only): run a tmpdir copy instead.
      fellBack = true;
      try {
        launch(copyToTmp());
      } catch (copyErr) {
        process.stderr.write(`[entry] ${copyErr instanceof Error ? copyErr.message : String(copyErr)}\n`);
        exitOnce(1);
      }
      return;
    }
    process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
    exitOnce(1);
  });
  proc.on('exit', (code) => {
    exitOnce(code ?? 1);
  });
}

for (const sig of ['SIGTERM', 'SIGINT']) {
  process.on(sig, () => {
    if (child !== null) child.kill(sig);
  });
}

launch(forgePath);
