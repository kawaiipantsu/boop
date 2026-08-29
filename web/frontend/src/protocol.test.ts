import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import {
  asRisk, chunkText, durationMs, errorText, parseAction, parseAgentList,
  parseApproval, parseEvent, parseModels, parseProviders, parseSessions,
  parseStatus, tokenText,
} from './protocol.js';

describe('parseEvent', () => {
  it('accepts a JSON string as delivered by the socket', () => {
    const ev = parseEvent('{"type":"model.token","session_id":"s1","payload":"hi","at":"2026-01-01T00:00:00Z"}');
    assert.deepEqual(ev, { type: 'model.token', session_id: 's1', payload: 'hi', at: '2026-01-01T00:00:00Z' });
  });

  it('rejects junk rather than throwing', () => {
    assert.equal(parseEvent('not json'), null);
    assert.equal(parseEvent('{"payload":1}'), null);
    assert.equal(parseEvent(42), null);
    assert.equal(parseEvent(null), null);
  });

  it('keeps an unknown event type so nothing is silently dropped', () => {
    assert.equal(parseEvent({ type: 'future.event' })?.type, 'future.event');
  });

  it('omits blank optional fields', () => {
    const ev = parseEvent({ type: 'error', session_id: '', agent_id: '' });
    assert.deepEqual(ev, { type: 'error' });
  });
});

describe('durationMs', () => {
  it('reads Go time.Duration nanoseconds', () => {
    assert.equal(durationMs(1_500_000_000), 1500);
    assert.equal(durationMs(0), 0);
  });

  it('reads Go duration strings including compound ones', () => {
    assert.equal(durationMs('1.5s'), 1500);
    assert.equal(durationMs('250ms'), 250);
    assert.equal(durationMs('1m30s'), 90_000);
    assert.equal(durationMs('2h5m'), 7_500_000);
    assert.equal(durationMs('900µs'), 0.9);
  });

  it('returns null when there is nothing to read', () => {
    assert.equal(durationMs(undefined), null);
    assert.equal(durationMs(''), null);
    assert.equal(durationMs('abc'), null);
  });
});

describe('parseAction', () => {
  it('maps permissions.Action verbatim', () => {
    const action = parseAction({
      category: 'shell.execute',
      risk: 'high',
      tool: 'run',
      summary: 'Run a shell command',
      detail: 'git push origin main',
      paths: ['/repo'],
      production: true,
    });
    assert.equal(action.category, 'shell.execute');
    assert.equal(action.risk, 'high');
    assert.equal(action.detail, 'git push origin main');
    assert.deepEqual(action.paths, ['/repo']);
    assert.equal(action.production, true);
  });

  it('falls back to medium for an unknown risk rather than pretending it is low', () => {
    assert.equal(asRisk('nonsense'), 'medium');
    assert.equal(asRisk(undefined), 'medium');
    assert.equal(asRisk('CRITICAL'), 'critical');
  });
});

describe('parseApproval', () => {
  it('reads a PendingApproval envelope', () => {
    const approval = parseApproval(
      {
        id: 'a-1',
        action: { category: 'shell.execute', risk: 'low', tool: 'run', summary: 'go test', detail: 'go test ./...' },
        decision: { outcome: 'confirm', reason: 'shell.execute requires confirmation', rule: 'confirm' },
        requested_at: '2026-01-01T10:00:00Z',
      },
      () => 'synthetic',
    );
    assert.ok(approval);
    assert.equal(approval.id, 'a-1');
    assert.equal(approval.reason, 'shell.execute requires confirmation');
    assert.equal(approval.action.detail, 'go test ./...');
  });

  it('accepts a bare Action, as internal/app/loop.go currently emits', () => {
    const approval = parseApproval(
      { category: 'filesystem.write', risk: 'medium', tool: 'write', summary: 'Write a file', detail: '/tmp/x' },
      () => 'synthetic-1',
    );
    assert.ok(approval);
    assert.equal(approval.id, 'synthetic-1');
    assert.equal(approval.action.tool, 'write');
  });

  it('rejects an empty payload', () => {
    assert.equal(parseApproval({}, () => 'x'), null);
    assert.equal(parseApproval(null, () => 'x'), null);
  });
});

describe('parseStatus', () => {
  it('reads the documented shape', () => {
    const s = parseStatus({
      version: 'v0.1.0-dev',
      provider: 'ollama',
      model: 'qwen2.5-coder',
      mode: 'confirm',
      session_id: 's1',
      project: { path: '/home/u/p' },
      agents: 3,
      tokens: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 },
    });
    assert.equal(s.provider, 'ollama');
    assert.equal(s.projectPath, '/home/u/p');
    assert.equal(s.agents, 3);
    assert.equal(s.tokens.total, 15);
  });

  it('survives an empty body', () => {
    const s = parseStatus(undefined);
    assert.equal(s.provider, '');
    assert.equal(s.tokens.total, 0);
    assert.deepEqual(s.pendingApprovals, []);
  });

  it('counts agents when the server sends the list instead of a number', () => {
    assert.equal(parseStatus({ agents: [{ id: 'a' }, { id: 'b' }] }).agents, 2);
  });

  it('picks up replayed pending approvals', () => {
    const s = parseStatus({
      pending_approvals: [{ id: 'p1', action: { tool: 'run', detail: 'ls', risk: 'low', category: 'shell.execute' } }],
    });
    assert.equal(s.pendingApprovals.length, 1);
    assert.equal(s.pendingApprovals[0]?.id, 'p1');
  });
});

describe('list parsers', () => {
  it('reads agents from an array or an envelope', () => {
    const fromArray = parseAgentList([{ id: 'a1', name: 'Coder', status: 'RUNNING' }]);
    assert.equal(fromArray[0]?.status, 'running');
    const fromEnvelope = parseAgentList({ agents: [{ id: 'a2', name: 'Tests' }] });
    assert.equal(fromEnvelope[0]?.name, 'Tests');
    assert.deepEqual(parseAgentList(null), []);
  });

  it('reads models and drops entries with no id', () => {
    const models = parseModels([
      { id: 'm1', provider: 'ollama', context_window: 8192, capabilities: ['tools'] },
      { provider: 'ollama' },
    ]);
    assert.equal(models.length, 1);
    assert.equal(models[0]?.contextWindow, 8192);
  });

  it('reads provider health from either a nested object or flat fields', () => {
    const providers = parseProviders([
      { name: 'lemonade', health: { ok: true, detail: 'up' } },
      { name: 'openai', healthy: false, error: 'no key' },
    ]);
    assert.equal(providers[0]?.healthy, true);
    assert.equal(providers[1]?.detail, 'no key');
  });

  it('reads sessions', () => {
    const sessions = parseSessions({ sessions: [{ id: 's1', title: 'Fix build', model: 'x' }] });
    assert.equal(sessions[0]?.title, 'Fix build');
  });
});

describe('payload text extraction', () => {
  it('reads model.token whether it is a bare string or an object', () => {
    assert.equal(tokenText('hello'), 'hello');
    assert.equal(tokenText({ text: 'hello' }), 'hello');
    assert.equal(tokenText({ delta: 'hi' }), 'hi');
    assert.equal(tokenText({ nope: 1 }), '');
  });

  it('reads command output chunks', () => {
    assert.equal(chunkText('line\n'), 'line\n');
    assert.equal(chunkText({ chunk: 'a' }), 'a');
    assert.equal(chunkText({ line: 'b' }), 'b');
  });

  it('always produces something for an error', () => {
    assert.equal(errorText({ message: 'boom' }), 'boom');
    assert.equal(errorText('boom'), 'boom');
    assert.match(errorText({}), /unspecified/);
  });
});
