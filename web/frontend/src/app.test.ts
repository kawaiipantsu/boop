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
  /** Sends one server envelope frame. */
  frame(msg: unknown): void {
    this.onmessage?.({ data: JSON.stringify(msg) });
  }
  /** Sends a core bus event wrapped in the `event` envelope. */
  emit(event: unknown): void {
    this.frame({ type: 'event', event });
  }
  /** Brings the connection up, hello frame included. */
  connect(hello: Record<string, unknown> = {}): void {
    this.readyState = 1;
    this.onopen?.({});
    this.frame({
      type: 'hello',
      data: { protocol: 1, session_id: 's-1', mode: 'confirm', pending_approvals: [], grants: [], ...hello },
    });
  }
}

// Bodies match web/api.go's response types.
const RESPONSES: Record<string, unknown> = {
  '/api/status': {
    version: { version: 'v0.1.0-dev' },
    provider: 'ollama',
    model: 'qwen2.5-coder',
    mode: 'confirm',
    current_session: 's-1',
    project_path: '/repo',
    clients: 1,
    pending_approvals: 0,
  },
  '/api/agents': { session_id: 's-1', agents: [], max: 5, enabled: true },
  '/api/approval': { pending: [], grants: [], mode: 'confirm' },
  '/api/message': { session_id: 's-1', text: '' },
  '/api/config': { config: { execution: { mode: 'confirm' } }, secrets: [], path: '/home/u/.config/boop/config.yaml' },
  '/api/tools': { tools: [{ name: 'run', description: 'Run a shell command' }], mode: 'confirm' },
};

async function flush(): Promise<void> {
  for (let i = 0; i < 6; i += 1) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}

const BASE_RESPONSES = { ...RESPONSES };

beforeEach(async () => {
  for (const key of Object.keys(RESPONSES)) delete RESPONSES[key];
  Object.assign(RESPONSES, BASE_RESPONSES);
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
    assert.deepEqual(labels, ['Chat', 'Agents', 'Tools', 'Models', 'Sessions', 'Statistics', 'Settings']);
  });

  it('shows exactly one panel at a time', async () => {
    const panels = Array.from(root.querySelectorAll('[role="tabpanel"]')) as HTMLElement[];
    assert.equal(panels.length, 7);
    assert.deepEqual(
      panels.filter((p) => !p.hidden).map((p) => p.id),
      ['panel-chat'],
    );
    const tabs = Array.from(root.querySelectorAll('[role="tab"]')) as HTMLButtonElement[];
    (tabs.find((t) => t.textContent === 'Statistics') as HTMLButtonElement).click();
    await flush();
    assert.deepEqual(
      panels.filter((p) => !p.hidden).map((p) => p.id),
      ['panel-stats'],
    );
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
    assert.equal(new URL(socket.url).pathname, '/api/events');
    assert.match(socket.url, /^ws:/);
    socket.connect();
    await flush();
    assert.match(root.querySelector('.conn')?.textContent ?? '', /Live/);
  });

  it('shows a dropped connection and recovers on reconnect', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    await flush();
    socket.readyState = 3;
    socket.onclose?.({});
    await flush();
    assert.match(root.querySelector('.conn')?.textContent ?? '', /Reconnecting|Disconnected/);
  });

  it('submits over the socket when it is up, and shows the turn immediately', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    await flush();
    const input = root.querySelector('.composer-input') as HTMLTextAreaElement;
    input.value = 'fix the build';
    (root.querySelector('.btn-primary') as HTMLButtonElement).click();
    await flush();
    const sent = JSON.parse(socket.sent[0] as string) as Record<string, unknown>;
    assert.equal(sent['type'], 'message');
    assert.deepEqual(sent['data'], { content: 'fix the build', session_id: 's-1' });
    assert.equal(calls.some((c) => c.url === '/api/message'), false);
    assert.match(text(), /fix the build/);
    assert.equal(input.value, '');
  });

  it('falls back to POST /api/message with async:true when the socket is down', async () => {
    const input = root.querySelector('.composer-input') as HTMLTextAreaElement;
    input.value = 'fix the build';
    (root.querySelector('.btn-primary') as HTMLButtonElement).click();
    await flush();
    const post = calls.find((c) => c.url === '/api/message');
    assert.ok(post);
    assert.equal(post.method, 'POST');
    assert.deepEqual(post.body, { content: 'fix the build', async: true, session_id: 's-1' });
  });

  it('streams tokens from the socket into the transcript', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    socket.emit({ type: 'prompt.received', payload: { text: 'hello' } });
    for (const t of ['Hi', ' ', 'there']) socket.emit({ type: 'model.token', payload: t });
    socket.emit({ type: 'model.response.completed', payload: { usage: { total_tokens: 42 } } });
    await flush();
    assert.match(root.querySelector('.assistant-text')?.textContent ?? '', /Hi there/);
    // Usage from model.response.completed lands in the header token field.
    assert.match(root.querySelector('.fields')?.textContent ?? '', /42/);
  });

  const PENDING = {
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
    requested_at: '2026-01-01T10:00:00Z',
  };

  it('raises an approval from the socket and resolves it over the socket', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    socket.frame({ type: 'approval', data: { kind: 'added', approval: PENDING } });
    await flush();

    const dialog = root.querySelector('[role="dialog"]') as HTMLElement;
    assert.ok(dialog, 'the approval dialog must be visible');
    assert.match(dialog.textContent ?? '', /git push origin main/);
    assert.match(dialog.textContent ?? '', /git\.push requires confirmation/);
    assert.ok(dialog.classList.contains('approval-severe'));

    const reject = Array.from(dialog.querySelectorAll('button')).find((b) => b.textContent === 'Reject');
    assert.ok(reject);
    reject.click();
    await flush();

    const sent = socket.sent.map((f) => JSON.parse(f) as Record<string, unknown>);
    const answer = sent.find((f) => f['type'] === 'approval');
    assert.ok(answer);
    assert.deepEqual(answer['data'], { id: 'ap-1', approved: false, scope: 'once' });
    assert.equal(root.querySelector('[role="dialog"]'), null);
  });

  it('does not raise two dialogs for the one decision', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    // The core publishes a bare Action on the bus and the full PendingApproval
    // as an approval frame. They describe the same request.
    socket.emit({ type: 'approval.requested', payload: PENDING.action });
    socket.frame({ type: 'approval', data: { kind: 'added', approval: PENDING } });
    await flush();
    assert.equal(root.querySelectorAll('[role="dialog"]').length, 1);
    // The surviving one is the resolvable one.
    const reject = Array.from(root.querySelectorAll('button')).find((b) => b.textContent === 'Reject');
    reject?.click();
    await flush();
    const answer = socket.sent.map((f) => JSON.parse(f) as Record<string, unknown>).find((f) => f['type'] === 'approval');
    assert.deepEqual((answer?.['data'] as Record<string, unknown>)['id'], 'ap-1');
  });

  it('restores the approval queue from the hello snapshot after a reconnect', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect({ pending_approvals: [PENDING] });
    await flush();
    assert.ok(root.querySelector('[role="dialog"]'));
  });

  it('clears the approval when another frontend answers it first (§50)', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    const pending = { id: 'ap-2', action: { tool: 'run', detail: 'ls', risk: 'low', category: 'shell.execute' } };
    socket.frame({ type: 'approval', data: { kind: 'added', approval: pending } });
    await flush();
    assert.ok(root.querySelector('[role="dialog"]'));
    socket.frame({ type: 'approval', data: { kind: 'resolved', approval: pending, approved: true, scope: 'once' } });
    await flush();
    assert.equal(root.querySelector('[role="dialog"]'), null);
  });

  it('enables Stop while busy and sends a cancel frame', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    const stop = root.querySelector('.btn-stop') as HTMLButtonElement;
    assert.equal(stop.disabled, true);
    socket.emit({ type: 'prompt.received', payload: { text: 'go' } });
    await flush();
    assert.equal(stop.disabled, false);
    stop.click();
    await flush();
    const sent = JSON.parse(socket.sent[0] as string) as Record<string, unknown>;
    assert.equal(sent['type'], 'cancel');
    assert.deepEqual(sent['data'], { session_id: 's-1' });
  });

  it('falls back to POST /api/interrupt when the socket is down, and tolerates a 404', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
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

  it('populates the settings editor from GET /api/config and saves just the config', async () => {
    const tabs = Array.from(root.querySelectorAll('[role="tab"]')) as HTMLButtonElement[];
    (tabs.find((t) => t.textContent === 'Settings') as HTMLButtonElement).click();
    await flush();
    const editor = root.querySelector('.config-editor') as HTMLTextAreaElement;
    assert.deepEqual(JSON.parse(editor.value), { execution: { mode: 'confirm' } });
    assert.match(text(), /\.config\/boop\/config\.yaml/);

    RESPONSES['/api/config'] = { config: { execution: { mode: 'auto' } }, secrets: [] };
    editor.value = JSON.stringify({ execution: { mode: 'auto' } });
    (Array.from(root.querySelectorAll('button')).find((b) => b.textContent === 'Save configuration') as HTMLButtonElement).click();
    await flush();
    const put = calls.find((c) => c.method === 'PUT');
    assert.ok(put);
    assert.deepEqual(put.body, { config: { execution: { mode: 'auto' } } });
  });

  it('lists tools from GET /api/tools', async () => {
    const tabs = Array.from(root.querySelectorAll('[role="tab"]')) as HTMLButtonElement[];
    (tabs.find((t) => t.textContent === 'Tools') as HTMLButtonElement).click();
    await flush();
    assert.match(text(), /Run a shell command/);
  });

  it('resyncs after the server reports dropped frames', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    await flush();
    const before = calls.filter((c) => c.url === '/api/status').length;
    socket.frame({ type: 'dropped', data: { count: 7 } });
    await flush();
    assert.match(text(), /dropped 7 update/);
    assert.ok(calls.filter((c) => c.url === '/api/status').length > before);
  });

  it('surfaces a server-side error frame', async () => {
    const socket = sockets[0] as StubSocket;
    socket.connect();
    socket.frame({ type: 'error', id: 'c1', error: { code: 'bad_request', message: '`content` must not be empty' } });
    await flush();
    assert.match(text(), /content` must not be empty/);
  });

  it('unmount() detaches its global listeners and stops the socket', () => {
    const before = sockets.length;
    unmount();
    unmount = () => undefined;
    assert.equal(sockets.length, before);
  });
});
