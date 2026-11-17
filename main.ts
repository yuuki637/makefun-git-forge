// Entry lifecycle for the git forge: serve on a kernel-picked port, tell the
// harness where we landed, then stay alive until the harness says the trial
// is over. The harness owns the schedule; this file only keeps the contract:
// register once (the first registration wins), poll /done, exit 0 after it.

import { createApp } from './app.ts';

const envBase = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envBase === '') {
  console.error('[forge] MAKEFUN_ENV_URL is not set, so there is no harness to register with');
  process.exit(1);
}

const FETCH_TIMEOUT_MS = 30_000;
const FIRST_RETRY_MS = 250;
const LONGEST_RETRY_MS = 2_000;

function pause(ms: number): Promise<void> {
  return new Promise((done) => setTimeout(done, ms));
}

async function announcePort(port: number): Promise<void> {
  let wait = FIRST_RETRY_MS;
  for (;;) {
    let refusal: Error | null = null;
    try {
      const reply = await fetch(envBase + '/register', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
      });
      if (reply.status === 200 || reply.status === 409) return;
      if (reply.status === 400 || reply.status === 405) {
        refusal = new Error('register refused with HTTP ' + reply.status + ': ' + (await reply.text()));
      }
    } catch {
      refusal = null; // a network hiccup just means the harness is not up yet
    }
    if (refusal !== null) throw refusal;
    await pause(wait);
    wait = Math.min(LONGEST_RETRY_MS, wait * 2);
  }
}

async function awaitTrialEnd(): Promise<void> {
  let wait = FIRST_RETRY_MS;
  for (;;) {
    try {
      const reply = await fetch(envBase + '/done', { signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
      if (reply.ok) {
        const state = (await reply.json()) as { done?: unknown };
        if (state.done === true) return;
      }
    } catch {
      // not answering yet is just another way of saying not done
    }
    await pause(wait);
    wait = Math.min(LONGEST_RETRY_MS, wait * 2);
  }
}

async function runForge(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const bound = server.address();
  if (bound === null || typeof bound === 'string') {
    throw new Error('the forge did not bind a TCP port');
  }
  console.log('[forge] serving on port ' + bound.port);
  await announcePort(bound.port);
  console.log('[forge] registered with the harness; waiting for done');
  await awaitTrialEnd();
  server.close();
  console.log('[forge] trial finished, shutting down');
}

runForge().then(
  () => process.exit(0),
  (err: unknown) => {
    console.error('[forge] ' + (err instanceof Error ? err.message : String(err)));
    process.exit(1);
  },
);
