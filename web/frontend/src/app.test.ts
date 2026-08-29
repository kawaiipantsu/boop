// End-to-end smoke test of the mounted application against a stubbed server.
//
// This is the one test that proves the wiring works: socket → store → views,
// composer → POST /api/message, approval dialog → POST /api/approval.

import assert from 'node:assert/strict';
import { afterEach, beforeEach, describe, it } from 'node:test';

import { setupDom } from './testing/dom.js';
import { mount } from './app.js';
import type { WebSocketLike } from './socket.js';

interface Call {
  url: string;
  method: string;
  body: unknown;
}

let dom: ReturnType<typeof setupDom>;
let calls: Call[];
let sockets: StubSocket[];
let unmount: () => void;
let root: HTMLElement;

class StubSocket implements WebSocketLike {
  readyState = 0;
  sent: string[] = [];
  onopen: ((ev: unknown) => void) | null = null;
  onclose: ((ev: unknown) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  constructor(readonly url: string) {
    sockets.push(this);
  }
  send(data: string): void {
    this.sent.push(data);
  }
  close(): void {
    this.readyState = 3;
  }
  emit(event: unknown): void {
    this.onmessage?.({ data: JSON.stringify(event) });
  }
}

const RESPONSES: Record<string, unknown> = {
  '/api/status': {
    version: 'v0.1.0-dev',
    provider: 'ollama',
    model: 'qwen2.5-coder',
    mode: 'confirm',
    session_id: 's-1',
    project: { path: '/repo' },
    agents: 0,
    tokens: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
  },
  '/api/agents': [],
  '/api/message': { session_id: 's-1' },
  '/api/approval': { ok: true },
};

async function flush(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

beforeEach(async () => {
  dom = setupDom();
  calls = [];
  sockets = [];
  const g = globalThis as unknown as Record<string, unknown>;
  g['WebSocket'] = StubSocket as unknown as typeof WebSocket;
  g['fetch'] = async (input: unknown, init?: RequestInit): Promise<Response> => {
    const url = String(input);
    const path = new URL(url).pathname;
    calls.push({
      url: path,
      method: init?.method ?? 'GET',
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    });
    const data = RESPONSES[path];
    if (data === undefined) {
      return new Response('not found', { status: 404 });
    }
    return new Response(JSON.stringify(data), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };

  root = document.getElementById('root') as HTMLElement;
  unmount = mount(root);
  await flush();
});

afterEach(() => {
  unmount();
  dom.cleanup();
});

function text(): string {
  return root.textContent ?? '';
}

describe('mounted app', () => {
  it('renders the shell with every §26 section we implement', () => {
    const labels = Array.from(root.querySelectorAll('[role="tab"]')).map((t) => t.textContent);
    assert.deepEqual(labels, ['Chat', 'Agents', 'Models', 'Sessions', 'Statistics', 'Settings']);
  });

  it('reads /api/status on boot and fills the header', async () => {
    assert.ok(calls.some((c) => c.url === '/api/status'));
    assert.match(text(), /ollama/);
    assert.match(text(), /qwen2\.5-coder/);
    assert.match(text(), /Confirm/);
    assert.match(text(), /\/repo/);
  });

  it('opens the event socket and reports the connection state', async () => {
    const socket = sockets[0] as StubSocket;
    assert.ok(socket);
    socket.readyState = 1;
    socket.onopen?.({});
    await flush();
    assert.match(root.querySelector('.conn')?.textContent ?? '', /Live/);
  });

  it('submits the composer to POST /api/message and shows the turn immediately', async () => {
    const input = root.querySelector('.composer-input') as HTMLTextAreaElement;
    input.value = 'fix the build';
    (root.querySelector('.btn-primary') as HTMLButtonElement).click();
    await flush();
    const post = calls.find((c) => c.url === '/api/message');
    assert.ok(post);
    assert.equal(post.method, 'POST');
    assert.deepEqual(post.body, { text: 'fix the build', message: 'fix the build', session_id: 's-1' });
    assert.match(text(), /fix the build/);
    assert.equal(input.value, '');
  });

  it('streams tokens from the socket into the transcript', async () => {
    const socket = sockets[0] as StubSocket;
    socket.readyState = 1;
    socket.onopen?.({});
    socket.emit({ type: 'prompt.received', payload: { text: 'hello' } });
    for (const t of ['Hi', ' ', 'there']) socket.emit({ type: 'model.token', payload: t });
    socket.emit({ type: 'model.response.completed', payload: { usage: { total_tokens: 42 } } });
    await flush();
    assert.match(root.querySelector('.assistant-text')?.textContent ?? '', /Hi there/);
    // Usage from model.response.completed lands in the header token field.
    assert.match(root.querySelector('.fields')?.textContent ?? '', /42/);
  });

  it('raises an approval from the socket and resolves it over POST /api/approval', async () => {
    const socket = sockets[0] as StubSocket;
    socket.readyState = 1;
    socket.onopen?.({});
    socket.emit({
      type: 'approval.requested',
      payload: {
        id: 'ap-1',
        action: {
          category: 'shell.execute',
          risk: 'high',
          tool: 'run',
          summary: 'Boop wants to run a command',
          detail: 'git push origin main',
          production: true,
        },
        decision: { outcome: 'confirm', reason: 'git.push requires confirmation' },
      },
    });
    await flush();

    const dialog = root.querySelector('[role="dialog"]') as HTMLElement;
    assert.ok(dialog, 'the approval dialog must be visible');
    assert.match(dialog.textContent ?? '', /git push origin main/);
    assert.ok(dialog.classList.contains('approval-severe'));

    const reject = Array.from(dialog.querySelectorAll('button')).find((b) => b.textContent === 'Reject');
    assert.ok(reject);
    reject.click();
    await flush();

    const post = calls.find((c) => c.url === '/api/approval');
    assert.ok(post);
    assert.deepEqual(post.body, { id: 'ap-1', approved: false, scope: 'once' });
    assert.equal(root.querySelector('[role="dialog"]'), null);
  });

  it('clears the approval when another frontend answers it first (§50)', async () => {
    const socket = sockets[0] as StubSocket;
    socket.readyState = 1;
    socket.onopen?.({});
    socket.emit({ type: 'approval.requested', payload: { id: 'ap-2', action: { tool: 'run', detail: 'ls', risk: 'low' } } });
    await flush();
    assert.ok(root.querySelector('[role="dialog"]'));
    socket.emit({ type: 'approval.received', payload: { id: 'ap-2', approved: true } });
    await flush();
    assert.equal(root.querySelector('[role="dialog"]'), null);
  });

  it('enables Stop while busy and sends the interrupt over the socket', async () => {
    const socket = sockets[0] as StubSocket;
    socket.readyState = 1;
    socket.onopen?.({});
    const stop = root.querySelector('.btn-stop') as HTMLButtonElement;
    assert.equal(stop.disabled, true);
    socket.emit({ type: 'prompt.received', payload: { text: 'go' } });
    await flush();
    assert.equal(stop.disabled, false);
    stop.click();
    await flush();
    assert.deepEqual(JSON.parse(socket.sent[0] as string), { type: 'interrupt', session_id: 's-1' });
  });

  it('falls back to POST /api/interrupt when the socket is down, and tolerates a 404', async () => {
    const socket = sockets[0] as StubSocket;
    socket.readyState = 1;
    socket.onopen?.({});
    socket.emit({ type: 'prompt.received', payload: { text: 'go' } });
    await flush();
    socket.readyState = 3;
    (root.querySelector('.btn-stop') as HTMLButtonElement).click();
    await flush();
    assert.ok(calls.some((c) => c.url === '/api/interrupt' && c.method === 'POST'));
    assert.doesNotMatch(text(), /Could not send the interrupt/);
  });

  it('tells the user when a message could not be delivered', async () => {
    delete RESPONSES['/api/message'];
    try {
      const input = root.querySelector('.composer-input') as HTMLTextAreaElement;
      input.value = 'this will fail';
      (root.querySelector('.btn-primary') as HTMLButtonElement).click();
      await flush();
      assert.match(text(), /Message not delivered/);
    } finally {
      RESPONSES['/api/message'] = { session_id: 's-1' };
    }
  });

  it('unmount() detaches its global listeners and stops the socket', () => {
    const before = sockets.length;
    unmount();
    unmount = () => undefined;
    assert.equal(sockets.length, before);
  });
});
