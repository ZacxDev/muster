// createAgent: the fixture path that replaced the web UI's dispatch form.
//
// docs/design/chief-dispatch.md phase 2 deleted the form (the sidebar's "+",
// "Dispatch an agent", a task card's Dispatch and "Save for later"). About
// fifteen specs used "Save for later" only to CREATE an agent; they call this
// instead. It POSTs /api/agents — the machine route that replaced the form —
// with start: false (exactly the row "Save for later" made: stopped, nothing
// provisioned) unless a spec asks for start: true, presenting a personal token
// of scope agent-create minted for the user (e2e/mktoken). So every run of
// those specs also exercises the new route.
import { execFileSync } from 'node:child_process';
import { expect } from '@playwright/test';
import { dsn, repoRoot } from './env';

export interface CreatedAgent {
  id: number;
  name: string;
  status: string;
  kind: string;
  workerPool?: string;
}

export interface CreateAgentOptions {
  kind?: 'gateway' | 'claude-code';
  prompt?: string;
  repo?: string;
  repo_branch?: string;
  model?: string;
  note_id?: number;
  cc_account?: string;
  cc_failover?: boolean;
  worker_pool?: string;
  start?: boolean;
  // user is whose agent it is (default 1, the operator).
  user?: number;
}

// One token per user per worker process. A spec that resets the accounts
// tables (seed --accounts-only) deletes it, so a 401 re-mints once.
const tokens = new Map<number, string>();

// agentCreateToken mints (or returns this process's) agent-create token for
// user, through the real token store as that user.
export function agentCreateToken(user = 1, fresh = false): string {
  const cached = tokens.get(user);
  if (cached && !fresh) return cached;
  const tok = execFileSync('go', ['run', './e2e/mktoken'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_USER: String(user) },
  })
    .toString()
    .trim();
  expect(tok, 'mktoken printed a personal token').toMatch(/^mst_/);
  tokens.set(user, tok);
  return tok;
}

// postAgents is one POST /api/agents as user's agent-create token, answered
// raw: for a spec asserting a REFUSAL (createAgent expects a 201).
export async function postAgents(origin: string, opts: CreateAgentOptions = {}): Promise<{ status: number; text: string }> {
  const { user = 1, start = false, ...fields } = opts;
  const body = JSON.stringify({ ...fields, start });
  const post = (tok: string) =>
    fetch(origin + '/api/agents', {
      method: 'POST',
      headers: { Authorization: `Bearer ${tok}`, 'Content-Type': 'application/json', Accept: 'application/json' },
      body,
    });
  let r = await post(agentCreateToken(user));
  if (r.status === 401) r = await post(agentCreateToken(user, true));
  return { status: r.status, text: await r.text() };
}

// createAgent creates one agent on the server at origin and returns its row.
export async function createAgent(origin: string, opts: CreateAgentOptions = {}): Promise<CreatedAgent> {
  const start = opts.start ?? false;
  const r = await postAgents(origin, opts);
  expect(r.status, `POST /api/agents: ${r.text}`).toBe(201);
  const out = JSON.parse(r.text) as { agent: CreatedAgent; started: boolean; href: string };
  expect(out.started).toBe(start);
  return out.agent;
}
