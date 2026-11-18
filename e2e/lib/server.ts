// Building and running real muster-server binaries for the browser suite.
//
// 🔴 THE BINARY IS BUILT WITH A VERSION, AND THAT IS THE POINT. The update
// spec needs two servers whose /sw.js bytes differ, and the only honest way to
// get that is two builds with different BuildVersion values: Playwright cannot
// route() a service worker's update fetch, so faking the second script is not
// an option.
import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { binDir, dsn, hookToken, password, repoRoot } from './env';

export function serverBinary(version: string): string {
  return path.join(binDir, `muster-server-${version}`);
}

// tags are Go build tags. Only the e2eterm binary (versionTerm) has one, and it
// is a separate binary so the specs that need none of its fixtures run the
// shipped build. (The GoTrue specs run it for one fixture: a short verifier
// skew, e2e/lib/gotrue.ts.)
export function buildServer(version: string, tags: string[] = []): string {
  fs.mkdirSync(binDir, { recursive: true });
  const out = serverBinary(version);
  const tagArgs = tags.length ? ['-tags', tags.join(',')] : [];
  execFileSync('go', ['build', ...tagArgs, '-ldflags', `-X github.com/ZacxDev/muster/internal/api.BuildVersion=${version}`, '-o', out, './cmd/muster-server'], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
  return out;
}

export function goRun(pkg: string, env: Record<string, string>): void {
  execFileSync('go', ['run', pkg], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, ...env } });
}

export interface Running {
  url: string;
  proc: ChildProcess;
  stop(): Promise<void>;
  // log is the server's output so far (stdout and stderr): a spec counts the
  // lines it writes for a socket's open and end.
  log(): string;
  // signIns is how many password sign-in attempts this server has judged so
  // far (each one drew from its throttle's bucket): see signInBudget.
  signIns(): number;
}

// --- The sign-in budget ------------------------------------------------------
//
// 🔴 THE RELATIONSHIP THIS GUARDS: password sign-in is throttled to
// loginRateBurst attempts per loginRateWindow in ONE server-wide bucket
// (internal/api/login.go), and a spec that signs in more often than that gets a
// 429 and times out on /login — but only when the runner is fast enough to fit
// them in one window, so it passes locally and fails CI. The check therefore
// counts what the SERVER judged over its WHOLE LIFE, which bounds every
// window's count and does not depend on the runner's speed. It reads the
// server's own log lines, so a sign-in counts however it was driven: through
// lib/signin.ts, or a form filled by hand.
//
// The limit is read from login.go itself, so the margin follows the server's
// constant rather than a copy of it. A server's lifetime count must stay at or
// under half of it.
//
// Counted: the password form's two judged outcomes (a session issued, a password
// rejected), and a REJECTED password step-up confirm. NOT counted: GoTrue-mode
// sign-in (it is not in this bucket) and a 429 (the test has already failed).
// ⚠ An ACCEPTED password step-up confirm draws from this bucket too but logs
// nothing (internal/api stepup.go confirmOperatorPassword), so it is invisible
// here: the half-burst margin is what absorbs them. Specs that drive one today:
// view-swap (3), composer-menu (2), mirrored (1 accepted, 1 rejected).
const loginGo = fs.readFileSync(path.join(repoRoot, 'internal', 'api', 'login.go'), 'utf8');
const burst = /\bloginRateBurst\s*=\s*(\d+)/.exec(loginGo);
if (!burst) throw new Error('internal/api/login.go: loginRateBurst not found — the sign-in budget cannot be set');
export const loginRateBurst = Number(burst[1]);
export const signInBudget = Math.floor(loginRateBurst / 2);
const signInLine = /login: (a browser session was issued|rejected a password attempt)|stepup: rejected an operator-password confirm/g;
export const countSignIns = (log: string) => (log.match(signInLine) ?? []).length;

// Every server this process started, and how many times lib/signin.ts drove the
// form against each. The second is the counter's POSITIVE CONTROL: a server the
// helper signed in to whose log shows fewer sign-ins than that is a counter that
// cannot see them (a reworded log line), which would otherwise read as a server
// that was never signed in to — a pass.
const started: { name: string; running: Running; formSignIns: number }[] = [];
const exited = (r: Running) => r.proc.exitCode !== null || r.proc.signalCode !== null;

// noteFormSignIn is lib/signin.ts reporting a form sign-in on origin. It is
// charged to the newest server on that origin that is still running.
export function noteFormSignIn(origin: string): void {
  for (let i = started.length - 1; i >= 0; i--) {
    if (started[i].running.url === origin && !exited(started[i].running)) {
      started[i].formSignIns++;
      return;
    }
  }
}

// signInBudgetBreaches lists every server this process started that has gone
// over the budget, and every STOPPED server whose log shows fewer sign-ins than
// the helper made on it (only a stopped one: a running server's last line may
// still be in the pipe). Each breach is reported once.
const reported = new Set<string>();
export function signInBudgetBreaches(): string[] {
  const out: string[] = [];
  for (const s of started) {
    const n = s.running.signIns();
    const msgs: string[] = [];
    if (n > signInBudget) {
      msgs.push(
        `${s.name} judged ${n} password sign-ins, over the budget of ${signInBudget} (half of login.go's loginRateBurst, ${loginRateBurst} a minute): sign in through lib/signin.ts, which reuses the session`,
      );
    }
    if (exited(s.running) && n < s.formSignIns) {
      msgs.push(`${s.name}: lib/signin.ts drove the form ${s.formSignIns} times but the server's log shows ${n} — the sign-in counter cannot see sign-ins (was the log line reworded?)`);
    }
    for (const m of msgs) {
      if (reported.has(m)) continue;
      reported.add(m);
      out.push(m);
    }
  }
  return out;
}

export function expectSignInBudget(): void {
  const b = signInBudgetBreaches();
  if (b.length) throw new Error('sign-in budget:\n  ' + b.join('\n  '));
}

export async function startServer(version: string, port: number, extraEnv: Record<string, string> = {}): Promise<Running> {
  const proc = spawn(serverBinary(version), [], {
    env: {
      ...process.env,
      DATABASE_URL: dsn,
      MUSTER_PORT: String(port),
      MUSTER_STANDALONE: '1',
      MUSTER_UI_PASSWORD: password,
      MUSTER_HOOK_TOKEN: hookToken,
      ...extraEnv,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let log = '';
  proc.stdout?.on('data', (d) => (log += d));
  proc.stderr?.on('data', (d) => (log += d));
  const url = `http://localhost:${port}`;
  const deadline = Date.now() + 20_000;
  for (;;) {
    if (proc.exitCode !== null) throw new Error(`muster-server ${version} exited ${proc.exitCode}:\n${log}`);
    try {
      const r = await fetch(url + '/health');
      if (r.ok) {
        const body = (await r.json()) as { version?: string };
        // The health check names the build, so a stale process on the port
        // (a previous run's) is caught here rather than tested by mistake.
        if (body.version !== version) throw new Error(`port ${port} answers version ${body.version}, want ${version}`);
        break;
      }
    } catch (e) {
      if (e instanceof Error && e.message.startsWith('port ')) throw e;
    }
    if (Date.now() > deadline) throw new Error(`muster-server ${version} did not come up on ${port}:\n${log}`);
    await new Promise((r) => setTimeout(r, 150));
  }
  const running: Running = {
    url,
    proc,
    log: () => log,
    signIns: () => countSignIns(log),
    async stop() {
      if (proc.exitCode !== null) return;
      const done = new Promise((r) => proc.once('exit', r));
      proc.kill('SIGTERM');
      await done;
    },
  };
  // Named by the spec that started it (the first spec file on the stack), so a
  // breach reported at the worker's end still says whose server it was.
  const by = /[\\/](tests[\\/][^\\/:]+\.spec\.ts)/.exec(new Error().stack ?? '')?.[1] ?? 'global-setup';
  started.push({ name: `${by}'s muster-server ${version} on :${port}`, running, formSignIns: 0 });
  return running;
}
