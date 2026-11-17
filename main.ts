/**
 * @file main.ts
 * @description Entrypoint for the git-forge server implementing the entry↔env lifecycle contract:
 * 1. Reads MAKEFUN_ENV_URL, validating it is set and non-empty.
 * 2. Creates the forge HTTP server and binds to an ephemeral port (port 0).
 *    The process never binds a fixed port because the env owns port 8080.
 * 3. Registers the assigned port with the environment via POST /register with retry/backoff.
 * 4. Polls GET /done until the environment signals completion.
 * 5. Closes the server and exits cleanly.
 */

import { createApp } from './app.ts';

/** Pauses execution for the specified milliseconds. */
async function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Calculates exponential backoff delay based on retry attempt, capped at 2000ms with exponent clamped at 8. */
function getRetryDelay(attempt: number): number {
  const exp = Math.min(attempt, 8);
  return Math.min(250 * (2 ** exp), 2000);
}

/** Registers the server port with the env server, retrying or failing according to contract. */
async function register(envUrl: string, port: number): Promise<void> {
  let attempt = 0;
  while (true) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(30000),
      });

      if (res.status === 200 || res.status === 409) {
        return;
      }

      if (res.status === 400 || res.status === 405) {
        throw new Error(`registration failed with status ${res.status}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('registration failed with status')) {
        throw err;
      }
    }

    const delay = getRetryDelay(attempt);
    await sleep(delay);
    attempt++;
  }
}

/** Polls the env server's /done endpoint until done === true. */
async function waitForDone(envUrl: string): Promise<void> {
  let attempt = 0;
  while (true) {
    try {
      const res = await fetch(`${envUrl}/done`, {
        method: 'GET',
        signal: AbortSignal.timeout(30000),
      });

      if (res.ok) {
        const data = (await res.json()) as { done?: boolean };
        if (data && data.done === true) {
          return;
        }
      }
    } catch {
      // Non-OK, network error, parse failure, or timeout mean not done; retry.
    }

    const delay = getRetryDelay(attempt);
    await sleep(delay);
    attempt++;
  }
}

/** Main lifecycle runner. */
async function main(): Promise<void> {
  const rawEnvUrl = process.env.MAKEFUN_ENV_URL;
  if (!rawEnvUrl || rawEnvUrl.trim() === '') {
    process.stderr.write('[entry] MAKEFUN_ENV_URL is unset or empty\n');
    process.exit(1);
  }

  const envUrl = rawEnvUrl.replace(/\/+$/, '');

  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.listen(0, () => resolve());
    server.on('error', reject);
  });

  const addr = server.address();
  if (!addr || typeof addr === 'string' || typeof addr.port !== 'number') {
    throw new Error('bound address is not a TCP port address');
  }

  const port = addr.port;
  process.stdout.write(`[entry] listening on port ${port}\n`);

  await register(envUrl, port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitForDone(envUrl);

  await new Promise<void>((resolve, reject) => {
    server.close((err) => {
      if (err) reject(err);
      else resolve();
    });
  });

  process.stdout.write('[entry] done\n');
  process.exit(0);
}

main().catch((err) => {
  const message = err instanceof Error ? err.message : String(err);
  process.stderr.write(`[entry] ${message}\n`);
  process.exit(1);
});
