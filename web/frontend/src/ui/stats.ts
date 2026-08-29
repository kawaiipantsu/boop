// Statistics view (§28), rendered from the stats.Snapshot returned by
// GET /api/stats. Parsing is tolerant: unknown buckets are skipped, missing
// ones simply do not render.

import { el, replaceChildren } from '../util/dom.js';
import { formatCost, formatDuration, formatNumber } from '../util/format.js';
import { durationMs, isRecord, num, str } from '../protocol.js';

interface Aggregate {
  key: string;
  tokens: { prompt: number; completion: number; total: number };
  counters: Record<string, number>;
  durations: Record<string, number | null>;
  cost: { measured: number; estimated: number; currency: string };
}

const COUNTER_LABELS: Array<[string, string]> = [
  ['messages', 'Messages'],
  ['model_calls', 'Model calls'],
  ['model_call_failures', 'Model call failures'],
  ['tool_calls', 'Tool calls'],
  ['tool_failures', 'Tool failures'],
  ['commands', 'Commands'],
  ['command_failures', 'Command failures'],
  ['command_timeouts', 'Command timeouts'],
  ['repair_iterations', 'Repair iterations'],
  ['repair_successes', 'Repair successes'],
  ['test_runs', 'Test runs'],
  ['tests_passed', 'Tests passed'],
  ['tests_failed', 'Tests failed'],
  ['agents_spawned', 'Agents spawned'],
  ['agents_completed', 'Agents completed'],
  ['agents_failed', 'Agents failed'],
];

const DURATION_LABELS: Array<[string, string]> = [
  ['model_ms', 'Model time'],
  ['tool_ms', 'Tool time'],
  ['command_ms', 'Command time'],
  ['test_ms', 'Test time'],
];

function tokensOf(raw: unknown): Aggregate['tokens'] {
  const r = isRecord(raw) ? raw : {};
  // Tokens may be a flat {prompt,completion,total} or {measured:{…},estimated:{…}}.
  const measured = isRecord(r['measured']) ? r['measured'] : null;
  const source = measured ?? r;
  const prompt = num(source['prompt'] ?? source['prompt_tokens']);
  const completion = num(source['completion'] ?? source['completion_tokens']);
  return { prompt, completion, total: num(source['total'] ?? source['total_tokens'], prompt + completion) };
}

export function parseAggregate(raw: unknown, key: string): Aggregate {
  const r = isRecord(raw) ? raw : {};
  const counters: Record<string, number> = {};
  const c = isRecord(r['counters']) ? r['counters'] : {};
  for (const [k, v] of Object.entries(c)) counters[k] = num(v);
  const durations: Record<string, number | null> = {};
  const d = isRecord(r['durations']) ? r['durations'] : {};
  for (const [k, v] of Object.entries(d)) durations[k] = durationMs(v);
  const cost = isRecord(r['cost']) ? r['cost'] : {};
  return {
    key: str(r['key'], key),
    tokens: tokensOf(r['tokens']),
    counters,
    durations,
    cost: {
      measured: num(cost['measured']),
      estimated: num(cost['estimated']),
      currency: str(cost['currency']),
    },
  };
}

function tile(label: string, value: string, note = ''): HTMLElement {
  return el(
    'div',
    { class: 'tile' },
    el('span', { class: 'tile-label', text: label }),
    el('span', { class: 'tile-value', text: value }),
    note !== '' ? el('span', { class: 'tile-note', text: note }) : null,
  );
}

function bucketTable(title: string, raw: unknown, currency: string): HTMLElement | null {
  if (!isRecord(raw)) return null;
  const rows = Object.entries(raw)
    .map(([k, v]) => parseAggregate(v, k))
    .filter((a) => a.tokens.total > 0 || Object.values(a.counters).some((n) => n > 0))
    .sort((a, b) => b.tokens.total - a.tokens.total)
    .slice(0, 25);
  if (rows.length === 0) return null;

  const body = el('tbody');
  for (const a of rows) {
    body.appendChild(
      el(
        'tr',
        {},
        el('td', {}, el('code', { text: a.key })),
        el('td', { class: 'num', text: formatNumber(a.tokens.total) }),
        el('td', { class: 'num', text: formatNumber(a.counters['model_calls'] ?? 0) }),
        el('td', { class: 'num', text: formatNumber(a.counters['tool_calls'] ?? 0) }),
        el('td', {
          class: 'num',
          text: formatCost(a.cost.measured + a.cost.estimated, a.cost.currency || currency),
        }),
      ),
    );
  }
  return el(
    'section',
    { class: 'stats-section' },
    el('h3', { class: 'panel-subtitle', text: title }),
    el(
      'table',
      { class: 'table' },
      el(
        'thead',
        {},
        el(
          'tr',
          {},
          el('th', { text: title.replace(/s$/, '') }),
          el('th', { class: 'num', text: 'Tokens' }),
          el('th', { class: 'num', text: 'Model calls' }),
          el('th', { class: 'num', text: 'Tool calls' }),
          el('th', { class: 'num', text: 'Cost' }),
        ),
      ),
      body,
    ),
  );
}

export class StatsPanel {
  readonly root: HTMLElement;
  private readonly body: HTMLElement;

  constructor(private readonly reload: () => void | Promise<void>) {
    this.body = el('div', { class: 'stats-body' }, el('p', { class: 'muted', text: 'Loading statistics…' }));
    this.root = el(
      'section',
      { class: 'panel panel-stats', aria: { label: 'Statistics' } },
      el(
        'div',
        { class: 'panel-head' },
        el('h2', { class: 'panel-title', text: 'Statistics' }),
        el('button', {
          class: 'btn btn-tiny',
          type: 'button',
          text: 'Refresh',
          on: { click: () => void this.reload() },
        }),
      ),
      this.body,
    );
  }

  setError(message: string): void {
    replaceChildren(this.body, [el('p', { class: 'notice notice-error', text: message })]);
  }

  render(raw: unknown): void {
    if (!isRecord(raw)) {
      this.setError('The server returned no statistics.');
      return;
    }
    const currency = str(raw['currency'], 'USD');
    const totals = parseAggregate(raw['totals'], 'totals');
    const uptime = durationMs(raw['uptime_ms'] ?? raw['uptime']);

    const tiles = el(
      'div',
      { class: 'tiles' },
      tile('Total tokens', formatNumber(totals.tokens.total),
        `${formatNumber(totals.tokens.prompt)} in / ${formatNumber(totals.tokens.completion)} out`),
      tile('Messages', formatNumber(totals.counters['messages'] ?? 0)),
      tile('Tool calls', formatNumber(totals.counters['tool_calls'] ?? 0),
        `${formatNumber(totals.counters['tool_failures'] ?? 0)} failed`),
      tile('Commands', formatNumber(totals.counters['commands'] ?? 0),
        `${formatNumber(totals.counters['command_failures'] ?? 0)} failed`),
      tile('Agents', formatNumber(totals.counters['agents_spawned'] ?? 0),
        `${formatNumber(totals.counters['agents_completed'] ?? 0)} completed`),
      tile('Cost', formatCost(totals.cost.measured + totals.cost.estimated, totals.cost.currency || currency),
        totals.cost.estimated > 0 ? `includes ${formatCost(totals.cost.estimated, currency)} estimated` : ''),
      tile('Uptime', formatDuration(uptime)),
    );

    const counterRows = el('tbody');
    for (const [key, label] of COUNTER_LABELS) {
      const value = totals.counters[key];
      if (value === undefined) continue;
      counterRows.appendChild(
        el('tr', {}, el('td', { text: label }), el('td', { class: 'num', text: formatNumber(value) })),
      );
    }
    const durationRows = el('tbody');
    for (const [key, label] of DURATION_LABELS) {
      const value = totals.durations[key];
      if (value === undefined || value === null) continue;
      durationRows.appendChild(
        el('tr', {}, el('td', { text: label }), el('td', { class: 'num', text: formatDuration(value) })),
      );
    }

    const sections: Array<HTMLElement | null> = [
      tiles,
      counterRows.childElementCount > 0
        ? el(
            'section',
            { class: 'stats-section' },
            el('h3', { class: 'panel-subtitle', text: 'Counters' }),
            el('table', { class: 'table' }, counterRows),
          )
        : null,
      durationRows.childElementCount > 0
        ? el(
            'section',
            { class: 'stats-section' },
            el('h3', { class: 'panel-subtitle', text: 'Time spent' }),
            el('table', { class: 'table' }, durationRows),
          )
        : null,
      bucketTable('Providers', raw['providers'], currency),
      bucketTable('Models', raw['models'], currency),
      bucketTable('Sessions', raw['sessions'], currency),
      bucketTable('Agents', raw['agents'], currency),
      bucketTable('Days', raw['days'], currency),
    ];

    const estimator = str(raw['estimator']);
    if (estimator !== '') {
      sections.push(
        el('p', { class: 'muted', text: `Estimated figures come from the "${estimator}" estimator.` }),
      );
    }
    replaceChildren(this.body, sections);
  }
}
