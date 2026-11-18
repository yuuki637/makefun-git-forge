// Entry lifecycle for the git forge: get a server listening on a
// kernel-picked port, tell the harness where it landed, then stay alive
// until the harness says the trial is over. The harness owns the schedule;
// this file only keeps the contract: register once (the first registration
// wins), poll /done, exit 0 after it.
//
// The serving itself is done by the Go core in forge.gz when it can be
// unpacked and started: it is several times faster than the Node server on
// the trial hardware. If anything about that path fails before the port is
// registered, this file falls back to the in-process Node server in app.ts,
// which implements the exact same wire behavior more slowly. Once a port is
// registered there is no switching: the first registration wins, so the
// fallback decision happens before announcePort is called.

import { spawn, type ChildProcess } from 'node:child_process';
import { gunzipSync } from 'node:zlib';
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createApp } from './app.ts';

const envBase = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envBase === '') {
  console.error('[forge] MAKEFUN_ENV_URL is not set, so there is no harness to register with');
  process.exit(1);
}

const FETCH_TIMEOUT_MS = 30_000;
const FIRST_RETRY_MS = 250;
const LONGEST_RETRY_MS = 2_000;
const GO_START_TIMEOUT_MS = 20_000;

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

interface RunningForge {
  port: number;
  stop: () => Promise<void>;
}

// Unpack forge.gz next to this file into a private temp dir and start it.
// The repo mount may be read-only or noexec, so the binary never runs in
// place. Resolves with the port from the child's `PORT <n>` line; rejects
// on any failure so the caller can fall back to the Node server.
function startGoForge(): Promise<RunningForge> {
  const here = dirname(fileURLToPath(import.meta.url));
  const packed = readFileSync(join(here, 'forge.gz'));
  const dir = mkdtempSync(join(tmpdir(), 'forge-core-'));
  const binary = join(dir, 'forge');
  writeFileSync(binary, gunzipSync(packed));
  chmodSync(binary, 0o755);

  return new Promise<RunningForge>((resolve, reject) => {
    const child: ChildProcess = spawn(binary, [], {
      env: { ...process.env, FORGE_PORT: '0' },
      stdio: ['ignore', 'pipe', 'inherit'],
    });
    let settled = false;
    let buffered = '';
    const timer = setTimeout(() => {
      fail(new Error('the Go core did not announce a port in time'));
    }, GO_START_TIMEOUT_MS);
    const fail = (err: Error): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      child.kill('SIGKILL');
      rmSync(dir, { recursive: true, force: true });
      reject(err);
    };
    child.once('error', (err) => fail(err));
    child.once('exit', (code) => {
      if (!settled) fail(new Error('the Go core exited early with code ' + String(code)));
    });
    child.stdout?.setEncoding('utf8');
    child.stdout?.on('data', (chunk: string) => {
      if (settled) return; // keep draining so the child never blocks on a full pipe
      buffered += chunk;
      const match = /PORT (\d+)/.exec(buffered);
      if (match) {
        settled = true;
        clearTimeout(timer);
        resolve({
          port: Number(match[1]),
          stop: async () => {
            child.kill('SIGTERM');
            rmSync(dir, { recursive: true, force: true });
          },
        });
      }
    });
  });
}

// The original Node server, kept as the safety net: identical wire
// behavior, started in-process on a kernel-picked port.
function startNodeForge(): Promise<RunningForge> {
  return new Promise<RunningForge>((resolve, reject) => {
    const server = createApp();
    server.once('error', reject);
    server.listen(0, () => {
      const bound = server.address();
      if (bound === null || typeof bound === 'string') {
        reject(new Error('the forge did not bind a TCP port'));
        return;
      }
      resolve({
        port: bound.port,
        stop: async () => {
          server.close();
        },
      });
    });
  });
}

async function runForge(): Promise<void> {
  let forge: RunningForge;
  try {
    forge = await startGoForge();
    console.log('[forge] Go core serving on port ' + forge.port);
  } catch (err) {
    console.error(
      '[forge] Go core unavailable (' + (err instanceof Error ? err.message : String(err)) + '); using the Node server',
    );
    forge = await startNodeForge();
    console.log('[forge] Node server serving on port ' + forge.port);
  }
  await announcePort(forge.port);
  console.log('[forge] registered with the harness; waiting for done');
  await awaitTrialEnd();
  await forge.stop();
  console.log('[forge] trial finished, shutting down');
}

runForge().then(
  () => process.exit(0),
  (err: unknown) => {
    console.error('[forge] ' + (err instanceof Error ? err.message : String(err)));
    process.exit(1);
  },
);
