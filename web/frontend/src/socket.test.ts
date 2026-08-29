import assert from 'node:assert/strict';
import { beforeEach, afterEach, describe, it } from 'node:test';

import { setupDom } from './testing/dom.js';
import { EventSocket, type ConnectionState, type WebSocketLike } from './socket.js';
import type { ApprovalEvent, BoopEvent, HelloData } from './protocol.js';

class FakeSocket implements WebSocketLike {
  static instances: FakeSocket[] = [];
  readyState = 0;
  sent: string[] = [];
  onopen: ((ev: unknown) => void) | null = null;
  onclose: ((ev: unknown) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;

  constructor(readonly url: string) {
    FakeSocket.instances.push(this);
  }
  send(data: string): void {
    this.sent.push(data);
  }
  close(): void {
    this.readyState = 3;
  }
  open(): void {
    this.readyState = 1;
    this.onopen?.({});
  }
  drop(): void {
    this.readyState = 3;
    this.onclose?.({});
  }
  deliver(data: unknown): void {
    this.onmessage?.({ data });
  }
}

interface Harness {
  socket: EventSocket;
  events: BoopEvent[];
  states: Array<[ConnectionState, number]>;
  hellos: HelloData[];
  approvals: ApprovalEvent[];
  errors: string[];
  dropped: number[];
  run: () => void;
  pending: () => number;
  opens: number;
}

function harness(): Harness {
  FakeSocket.instances = [];
  const events: BoopEvent[] = [];
  const states: Array<[ConnectionState, number]> = [];
  const hellos: HelloData[] = [];
  const approvals: ApprovalEvent[] = [];
  const errors: string[] = [];
  const dropped: number[] = [];
  let timers: Array<() => void> = [];
  const h: Partial<Harness> = { events, states, hellos, approvals, errors, dropped, opens: 0 };

  const socket = new EventSocket({
    onEvent: (ev) => events.push(ev),
    onState: (state, detail) => states.push([state, detail.nextRetryMs]),
    onHello: (hello) => hellos.push(hello),
    onApproval: (ev) => approvals.push(ev),
    onServerError: (msg) => errors.push(msg),
    onDropped: (count) => dropped.push(count),
    onOpen: () => {
      h.opens = (h.opens ?? 0) + 1;
    },
    factory: (url) => new FakeSocket(url),
    setTimer: (fn) => {
      timers.push(fn);
      return timers.length;
    },
    clearTimer: () => undefined,
    random: () => 0.5,
  });

  return Object.assign(h, {
    socket,
    run: () => {
      const queued = timers;
      timers = [];
      for (const fn of queued) fn();
    },
    pending: () => timers.length,
  }) as Harness;
}

describe('EventSocket', () => {
  let dom: ReturnType<typeof setupDom>;
  beforeEach(() => {
    dom = setupDom();
  });
  afterEach(() => dom.cleanup());

  it('reports connecting then open, and derives a ws:// url', () => {
    const h = harness();
    h.socket.start();
    assert.deepEqual(h.states[0], ['connecting', 0]);
    const s = FakeSocket.instances[0];
    assert.ok(s);
    assert.match(s.url, /^ws:\/\/127\.0\.0\.1:8585\//);
    s.open();
    assert.deepEqual(h.states[1], ['open', 0]);
    assert.equal(h.socket.connected, true);
  });

  it('unwraps the server envelope and drops malformed frames', () => {
    const h = harness();
    h.socket.start();
    const s = FakeSocket.instances[0] as FakeSocket;
    s.open();
    s.deliver('{"type":"event","event":{"type":"model.token","payload":"a"}}');
    s.deliver('<<not json>>');
    s.deliver('{"type":"event"}');
    s.deliver('{"type":"model.token","payload":"a"}'); // bare event, not the envelope
    s.deliver('{"type":"something.new"}');
    assert.equal(h.events.length, 1);
    assert.equal(h.events[0]?.type, 'model.token');
  });

  it('routes hello, approval, error and dropped frames', () => {
    const h = harness();
    h.socket.start();
    const s = FakeSocket.instances[0] as FakeSocket;
    s.open();
    s.deliver(JSON.stringify({
      type: 'hello',
      data: {
        protocol: 1,
        session_id: 's-1',
        mode: 'confirm',
        pending_approvals: [{ id: 'p1', action: { tool: 'run', detail: 'ls' } }],
      },
    }));
    assert.equal(h.hellos.length, 1);
    assert.equal(h.hellos[0]?.sessionId, 's-1');
    assert.equal(h.hellos[0]?.pendingApprovals[0]?.id, 'p1');

    s.deliver(JSON.stringify({
      type: 'approval',
      data: { kind: 'added', approval: { id: 'p2', action: { tool: 'write', detail: '/tmp/x' } } },
    }));
    assert.equal(h.approvals[0]?.kind, 'added');
    assert.equal(h.approvals[0]?.approval?.id, 'p2');

    s.deliver(JSON.stringify({ type: 'error', id: 'c1', error: { code: 'bad_request', message: 'nope' } }));
    assert.deepEqual(h.errors, ['nope']);

    s.deliver(JSON.stringify({ type: 'dropped', data: { count: 12 } }));
    assert.deepEqual(h.dropped, [12]);

    // ack and pong are accepted and ignored.
    s.deliver(JSON.stringify({ type: 'ack', id: 'c1', data: {} }));
    s.deliver(JSON.stringify({ type: 'pong', id: 'c2' }));
    assert.equal(h.errors.length, 1);
  });

  it('warns when the server speaks a different envelope version', () => {
    const h = harness();
    h.socket.start();
    const s = FakeSocket.instances[0] as FakeSocket;
    s.open();
    s.deliver(JSON.stringify({ type: 'hello', data: { protocol: 99 } }));
    assert.match(h.errors[0] ?? '', /protocol 1 but the server speaks 99/);
  });

  it('backs off exponentially and caps the delay', () => {
    const h = harness();
    assert.equal(h.socket.delayFor(1), 500);
    assert.equal(h.socket.delayFor(2), 1000);
    assert.equal(h.socket.delayFor(3), 2000);
    assert.equal(h.socket.delayFor(20), 30_000);
  });

  it('reconnects after a drop and resets the backoff once open', () => {
    const h = harness();
    h.socket.start();
    const first = FakeSocket.instances[0] as FakeSocket;
    first.open();
    first.drop();
    assert.equal(h.pending(), 1);
    h.run();
    assert.equal(FakeSocket.instances.length, 2);
    const second = FakeSocket.instances[1] as FakeSocket;
    second.open();
    assert.equal(h.opens, 2);
    second.drop();
    // Backoff restarted from the first step rather than continuing to grow.
    const last = h.states[h.states.length - 1];
    assert.equal(last?.[1], 500);
  });

  it('always uses web.EventsPath', () => {
    const h = harness();
    h.socket.start();
    (FakeSocket.instances[0] as FakeSocket).drop();
    h.run();
    const paths = FakeSocket.instances.map((s) => new URL(s.url).pathname);
    assert.deepEqual(paths, ['/api/events', '/api/events']);
  });

  it('surfaces offline after repeated failures', () => {
    const h = harness();
    h.socket.start();
    (FakeSocket.instances[0] as FakeSocket).drop();
    h.run();
    (FakeSocket.instances[1] as FakeSocket).drop();
    assert.equal(h.states.some(([s]) => s === 'offline'), true);
  });

  it('sends the client envelope only while open, and reports failure otherwise', () => {
    const h = harness();
    h.socket.start();
    assert.equal(h.socket.send('cancel', {}), false);
    const s = FakeSocket.instances[0] as FakeSocket;
    s.open();
    assert.equal(h.socket.send('cancel', { session_id: 's1' }), true);
    assert.deepEqual(JSON.parse(s.sent[0] as string), { type: 'cancel', id: 'c1', data: { session_id: 's1' } });
    h.socket.send('ping');
    assert.deepEqual(JSON.parse(s.sent[1] as string), { type: 'ping', id: 'c2' });
  });

  it('stop() prevents any further reconnection', () => {
    const h = harness();
    h.socket.start();
    const s = FakeSocket.instances[0] as FakeSocket;
    s.open();
    h.socket.stop();
    s.drop();
    assert.equal(h.pending(), 0);
    assert.equal(FakeSocket.instances.length, 1);
  });

  it('retryNow() skips the wait', () => {
    const h = harness();
    h.socket.start();
    (FakeSocket.instances[0] as FakeSocket).drop();
    h.socket.retryNow();
    assert.equal(FakeSocket.instances.length, 2);
  });
});
