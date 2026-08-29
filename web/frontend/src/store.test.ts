import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { Store, emptyStatus } from './store.js';
import type { BoopEvent } from './protocol.js';

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
