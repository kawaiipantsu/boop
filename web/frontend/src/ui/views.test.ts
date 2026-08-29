import assert from 'node:assert/strict';
import { afterEach, beforeEach, describe, it } from 'node:test';

import { setupDom } from '../testing/dom.js';
import { emptyStatus, Store } from '../store.js';
import type { AppState } from '../store.js';
import { AgentsPanel } from './agents.js';
import { Header } from './header.js';
import { StatsPanel } from './stats.js';
import { Shell } from './shell.js';
import { el } from '../util/dom.js';

let dom: ReturnType<typeof setupDom>;
beforeEach(() => {
  dom = setupDom();
});
afterEach(() => dom.cleanup());

function state(over: Partial<AppState> = {}): AppState {
  return {
    connection: 'open',
    nextRetryMs: 0,
    status: emptyStatus(),
    agents: [],
    approvals: [],
    busy: false,
    lastError: '',
    ...over,
  };
}

describe('Header', () => {
  it('shows provider, model, mode, tokens and agent count', () => {
    const header = new Header(() => undefined);
    header.update(
      state({
        status: {
          ...emptyStatus(),
          provider: 'lemonade',
          model: 'qwen2.5-coder',
          mode: 'confirm',
          projectPath: '/home/u/boop',
          tokens: { prompt: 900, completion: 600, total: 1500 },
        },
        agents: [
          { id: 'a', name: 'A', status: 'working', task: '', model: '', operation: '', tools: [], files: [], tokens: 0, runtimeMs: null, output: '' },
        ],
      }),
    );
    const text = header.root.textContent ?? '';
    assert.match(text, /lemonade/);
    assert.match(text, /qwen2\.5-coder/);
    assert.match(text, /Confirm/);
    assert.match(text, /1\.5k/);
    assert.match(text, /\/home\/u\/boop/);
  });

  it('makes a dropped socket obvious and offers a manual retry', () => {
    const header = new Header(() => undefined);
    header.update(state({ connection: 'open' }));
    const conn = header.root.querySelector('.conn') as HTMLElement;
    const retry = header.root.querySelector('.btn-tiny') as HTMLButtonElement;
    assert.equal(conn.dataset['state'], 'open');
    assert.equal(retry.hidden, true);

    header.update(state({ connection: 'offline', nextRetryMs: 4000 }));
    assert.equal(conn.dataset['state'], 'offline');
    assert.match(conn.textContent ?? '', /Disconnected/);
    assert.match(conn.textContent ?? '', /retrying in 4\.00s/);
    assert.equal(retry.hidden, false);
    assert.equal(conn.getAttribute('aria-live'), 'polite');
  });

  it('fires the retry callback', () => {
    let retried = 0;
    const header = new Header(() => (retried += 1));
    header.update(state({ connection: 'offline' }));
    (header.root.querySelector('.btn-tiny') as HTMLButtonElement).click();
    assert.equal(retried, 1);
  });
});

describe('AgentsPanel', () => {
  const agent = (id: string, status: string, over: Record<string, unknown> = {}) => ({
    id,
    name: id,
    status,
    task: `task for ${id}`,
    model: 'qwen',
    operation: 'editing',
    tools: ['read', 'run'],
    files: ['main.go'],
    tokens: 1234,
    runtimeMs: 65_000,
    output: 'ok\n',
    ...over,
  });

  it('lists agents with their status', () => {
    const panel = new AgentsPanel();
    panel.update([agent('Coder', 'working'), agent('Reviewer', 'waiting')]);
    const rows = panel.root.querySelectorAll('.agent-row');
    assert.equal(rows.length, 2);
    assert.match(rows[0]?.textContent ?? '', /WORKING/);
    assert.ok((rows[0] as HTMLElement).classList.contains('is-working'));
    assert.ok((rows[1] as HTMLElement).classList.contains('is-waiting'));
  });

  it('shows the §26 detail set for the selected agent', () => {
    const panel = new AgentsPanel();
    panel.update([agent('Coder', 'working')]);
    const detail = panel.root.querySelector('.agent-detail')?.textContent ?? '';
    for (const expected of ['task for Coder', 'WORKING', 'qwen', 'editing', 'read, run', 'main.go', '1,234', '1m 05s']) {
      assert.match(detail, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
    }
  });

  it('keeps the selection across updates and switches on click', () => {
    const panel = new AgentsPanel();
    panel.update([agent('Coder', 'working'), agent('Tests', 'testing')]);
    const rows = () => Array.from(panel.root.querySelectorAll('.agent-row')) as HTMLButtonElement[];
    rows()[1]?.click();
    assert.match(panel.root.querySelector('.agent-detail')?.textContent ?? '', /task for Tests/);
    panel.update([agent('Coder', 'working'), agent('Tests', 'complete')]);
    assert.match(panel.root.querySelector('.agent-detail')?.textContent ?? '', /task for Tests/);
  });

  it('says so when there are no agents', () => {
    const panel = new AgentsPanel();
    panel.update([]);
    assert.match(panel.root.textContent ?? '', /No agents/);
  });

  it('renders agent output as text', () => {
    const panel = new AgentsPanel();
    panel.update([agent('X', 'working', { output: '<script>bad()</script>' })]);
    assert.equal(panel.root.querySelectorAll('script').length, 0);
  });
});

describe('StatsPanel', () => {
  it('renders a stats.Snapshot', () => {
    const panel = new StatsPanel(() => undefined);
    panel.render({
      generated_at: '2026-01-01T00:00:00Z',
      uptime_ms: 3_600_000_000_000,
      estimator: 'heuristic',
      currency: 'USD',
      totals: {
        tokens: { prompt: 1000, completion: 500, total: 1500 },
        counters: { messages: 4, tool_calls: 9, tool_failures: 1, commands: 3, command_failures: 1, agents_spawned: 2, agents_completed: 2 },
        durations: { model_ms: 4_000_000_000, command_ms: 250_000_000 },
        cost: { currency: 'USD', measured: 0, estimated: 0.0123 },
      },
      providers: { ollama: { key: 'ollama', tokens: { total: 1500 }, counters: { model_calls: 4, tool_calls: 9 }, cost: { measured: 0, estimated: 0 } } },
      models: {},
    });
    const text = panel.root.textContent ?? '';
    assert.match(text, /1,500/);
    assert.match(text, /Tool calls/);
    assert.match(text, /ollama/);
    assert.match(text, /1h 00m/);
    assert.match(text, /0\.0123 USD/);
    assert.match(text, /heuristic/);
  });

  it('degrades gracefully on a missing or empty body', () => {
    const panel = new StatsPanel(() => undefined);
    panel.render(null);
    assert.match(panel.root.textContent ?? '', /no statistics/);
    panel.render({});
    assert.match(panel.root.textContent ?? '', /Total tokens/);
  });
});

describe('Shell', () => {
  it('exposes tabs with correct ARIA and shows only the active panel', () => {
    const a = el('section', { text: 'A' });
    const b = el('section', { text: 'B' });
    const shell = new Shell(el('header'), el('div'), [
      { id: 'chat', label: 'Chat', node: a },
      { id: 'agents', label: 'Agents', node: b },
    ]);
    document.body.appendChild(shell.root);
    assert.equal(shell.current, 'chat');
    assert.equal(a.hidden, false);
    assert.equal(b.hidden, true);
    const tabs = Array.from(shell.root.querySelectorAll('[role="tab"]')) as HTMLButtonElement[];
    assert.equal(tabs[0]?.getAttribute('aria-selected'), 'true');
    assert.equal(tabs[0]?.getAttribute('aria-controls'), 'panel-chat');
    assert.equal(a.getAttribute('role'), 'tabpanel');
  });

  it('moves between tabs with the arrow keys', () => {
    const shell = new Shell(el('header'), el('div'), [
      { id: 'chat', label: 'Chat', node: el('section') },
      { id: 'agents', label: 'Agents', node: el('section') },
    ]);
    document.body.appendChild(shell.root);
    const tablist = shell.root.querySelector('[role="tablist"]') as HTMLElement;
    tablist.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }));
    assert.equal(shell.current, 'agents');
    tablist.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Home', bubbles: true }));
    assert.equal(shell.current, 'chat');
  });

  it('calls onShow when a tab becomes active', () => {
    let shown = 0;
    const shell = new Shell(el('header'), el('div'), [
      { id: 'chat', label: 'Chat', node: el('section') },
      { id: 'stats', label: 'Statistics', node: el('section'), onShow: () => (shown += 1) },
    ]);
    assert.equal(shown, 0);
    shell.select('stats');
    assert.equal(shown, 1);
  });
});

describe('Store + panels integration', () => {
  it('drives the agents panel from bus events', () => {
    const store = new Store();
    const panel = new AgentsPanel();
    store.subscribe((s) => panel.update(s.agents));
    store.apply({ type: 'agent.created', payload: { id: 'a1', name: 'Research', status: 'waiting' } });
    store.apply({ type: 'agent.status.changed', payload: { id: 'a1', status: 'complete' } });
    assert.match(panel.root.querySelector('.agent-row')?.textContent ?? '', /COMPLETE/);
  });
});
