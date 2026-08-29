import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { Store, emptyStatus } from './store.js';
import { parseApproval } from './protocol.js';
import type { Approval, BoopEvent } from './protocol.js';

function approval(over: Record<string, unknown>): Approval {
  const parsed = parseApproval(over, () => 'synthetic');
  if (!parsed) throw new Error('fixture did not parse');
  return parsed;
}

function feed(store: Store, events: BoopEvent[]): void {
  for (const ev of events) store.apply(ev);
}

describe('Store', () => {
  it('tracks busy across a turn', () => {
    const store = new Store();
    assert.equal(store.get().busy, false);
    store.apply({ type: 'prompt.received', payload: { text: 'hi' } });
    assert.equal(store.get().busy, true);
    store.apply({ type: 'session.completed' });
    assert.equal(store.get().busy, false);
  });

  it('clears busy on an error so the composer does not stay locked', () => {
    const store = new Store();
    store.apply({ type: 'prompt.received' });
    store.apply({ type: 'error', payload: { message: 'provider down' } });
    assert.equal(store.get().busy, false);
  });

  it('accumulates usage across model responses', () => {
    const store = new Store();
    feed(store, [
      { type: 'model.response.completed', payload: { usage: { prompt_tokens: 10, completion_tokens: 4, total_tokens: 14 } } },
      { type: 'model.response.completed', payload: { usage: { prompt_tokens: 20, completion_tokens: 6, total_tokens: 26 } } },
    ]);
    assert.deepEqual(store.get().status.tokens, { prompt: 30, completion: 10, total: 40 });
  });

  it('queues approvals and clears them by id', () => {
    const store = new Store();
    store.apply({
      type: 'approval.requested',
      payload: { id: 'a1', action: { tool: 'run', detail: 'ls', risk: 'low', category: 'shell.execute' } },
    });
    assert.equal(store.get().approvals.length, 1);
    store.apply({ type: 'approval.received', payload: { id: 'a1', approved: true } });
    assert.equal(store.get().approvals.length, 0);
  });

  it('clears an id-less approval by tool name, which is what loop.go emits', () => {
    const store = new Store();
    store.apply({ type: 'approval.requested', payload: { tool: 'run', detail: 'ls', risk: 'low', category: 'shell.execute' } });
    store.apply({ type: 'approval.requested', payload: { tool: 'write', detail: '/tmp/a', risk: 'medium', category: 'filesystem.write' } });
    assert.equal(store.get().approvals.length, 2);
    store.apply({ type: 'approval.received', payload: { tool: 'write', approved: false } });
    const left = store.get().approvals;
    assert.equal(left.length, 1);
    assert.equal(left[0]?.action.tool, 'run');
  });

  it('never queues the same approval id twice', () => {
    const store = new Store();
    const ev: BoopEvent = { type: 'approval.requested', payload: { id: 'dup', action: { tool: 'run', detail: 'ls' } } };
    store.apply(ev);
    store.apply(ev);
    assert.equal(store.get().approvals.length, 1);
  });

  it('upserts agents from agent.created and agent.status.changed', () => {
    const store = new Store();
    store.apply({ type: 'agent.created', payload: { id: 'a1', name: 'Coder', status: 'waiting', model: 'qwen' } });
    store.apply({ type: 'agent.status.changed', payload: { id: 'a1', status: 'working', operation: 'editing main.go' } });
    const agents = store.get().agents;
    assert.equal(agents.length, 1);
    assert.equal(agents[0]?.status, 'working');
    // A partial update must not wipe fields the earlier event established.
    assert.equal(agents[0]?.model, 'qwen');
    assert.equal(agents[0]?.name, 'Coder');
  });

  it('takes the agent id from the envelope when the payload omits it', () => {
    const store = new Store();
    store.apply({ type: 'agent.status.changed', agent_id: 'a9', payload: { status: 'complete' } });
    assert.equal(store.get().agents[0]?.id, 'a9');
  });

  it('does not let a stale status poll rewind token counts', () => {
    const store = new Store();
    store.apply({ type: 'model.response.completed', payload: { usage: { total_tokens: 100 } } });
    store.setStatus({ ...emptyStatus(), provider: 'ollama', tokens: { prompt: 0, completion: 0, total: 0 } });
    assert.equal(store.get().status.tokens.total, 100);
    assert.equal(store.get().status.provider, 'ollama');
  });

  it('keeps socket-delivered approvals when a status poll lands', () => {
    const store = new Store();
    store.apply({ type: 'approval.requested', payload: { id: 'a1', action: { tool: 'run', detail: 'ls' } } });
    store.setStatus(emptyStatus());
    assert.equal(store.get().approvals.length, 1);
  });

  it('notifies subscribers immediately and on change', () => {
    const store = new Store();
    const seen: string[] = [];
    const off = store.subscribe((s) => seen.push(s.connection));
    assert.deepEqual(seen, ['connecting']);
    store.setConnection('open');
    store.setConnection('open');
    assert.deepEqual(seen, ['connecting', 'open']);
    off();
    store.setConnection('offline');
    assert.equal(seen.length, 2);
  });

  it('ignores unknown event types', () => {
    const store = new Store();
    const before = store.get();
    store.apply({ type: 'something.new', payload: { a: 1 } });
    assert.equal(store.get(), before);
  });
});

describe('Store approval queue', () => {
  const ACTION = { category: 'shell.execute', risk: 'high', tool: 'run', summary: 'push', detail: 'git push origin main' };

  it('pairs the bare bus Action with the id-carrying approval frame', () => {
    const store = new Store();
    // The bus event has no id, so the UI could not answer it on its own.
    store.apply({ type: 'approval.requested', payload: ACTION });
    assert.equal(store.get().approvals.length, 1);
    assert.equal(store.get().approvals[0]?.synthetic, true);

    store.applyApprovalEvent({ kind: 'added', approval: approval({ id: 'ap-1', action: ACTION }), approved: false, scope: '' });
    const queue = store.get().approvals;
    assert.equal(queue.length, 1, 'one decision must not become two dialogs');
    assert.equal(queue[0]?.id, 'ap-1');
    assert.equal(queue[0]?.synthetic, false);
  });

  it('does not downgrade a real approval back to a synthetic one', () => {
    const store = new Store();
    store.applyApprovalEvent({ kind: 'added', approval: approval({ id: 'ap-1', action: ACTION }), approved: false, scope: '' });
    store.apply({ type: 'approval.requested', payload: ACTION });
    assert.equal(store.get().approvals.length, 1);
    assert.equal(store.get().approvals[0]?.id, 'ap-1');
  });

  it('keeps distinct actions apart', () => {
    const store = new Store();
    store.applyApprovalEvent({ kind: 'added', approval: approval({ id: 'a', action: ACTION }), approved: false, scope: '' });
    store.applyApprovalEvent({
      kind: 'added',
      approval: approval({ id: 'b', action: { ...ACTION, detail: 'git push --force' } }),
      approved: false,
      scope: '',
    });
    assert.equal(store.get().approvals.length, 2);
  });

  it('clears on a resolved or cancelled frame', () => {
    const store = new Store();
    const a = approval({ id: 'ap-1', action: ACTION });
    store.applyApprovalEvent({ kind: 'added', approval: a, approved: false, scope: '' });
    store.applyApprovalEvent({ kind: 'resolved', approval: a, approved: true, scope: 'once' });
    assert.equal(store.get().approvals.length, 0);
  });

  it('takes session, mode and the pending queue from hello', () => {
    const store = new Store();
    store.applyHello({
      protocol: 1,
      sessionId: 's-9',
      mode: 'auto',
      pendingApprovals: [approval({ id: 'ap-1', action: ACTION })],
    });
    assert.equal(store.get().status.sessionId, 's-9');
    assert.equal(store.get().status.mode, 'auto');
    assert.equal(store.get().approvals.length, 1);
  });

  it('discards a stale GET /api/approval response', () => {
    const store = new Store();
    const epoch = store.approvalsEpoch();
    // The socket delivers an approval while the poll is in flight.
    store.applyApprovalEvent({ kind: 'added', approval: approval({ id: 'ap-1', action: ACTION }), approved: false, scope: '' });
    store.setApprovals([], epoch);
    assert.equal(store.get().approvals.length, 1, 'the stale empty response must not erase it');

    // A response captured after that change still applies.
    store.setApprovals([], store.approvalsEpoch());
    assert.equal(store.get().approvals.length, 0);
  });
});
