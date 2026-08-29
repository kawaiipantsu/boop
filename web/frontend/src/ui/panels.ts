// The smaller navigation panels: Models (with provider health), Sessions and
// Settings. Each one is a thin read/refresh view over a single endpoint.

import { el, replaceChildren } from '../util/dom.js';
import { formatDateTime, formatNumber } from '../util/format.js';
import type { ModelView, ProviderView, SessionView } from '../protocol.js';

function panelShell(
  title: string,
  body: HTMLElement,
  onRefresh: () => void | Promise<void>,
  extra?: HTMLElement,
): HTMLElement {
  return el(
    'section',
    { class: 'panel', aria: { label: title } },
    el(
      'div',
      { class: 'panel-head' },
      el('h2', { class: 'panel-title', text: title }),
      extra ?? null,
      el('button', { class: 'btn btn-tiny', type: 'button', text: 'Refresh', on: { click: () => void onRefresh() } }),
    ),
    body,
  );
}

export class ModelsPanel {
  readonly root: HTMLElement;
  private readonly body = el('div', { class: 'panel-body' });

  constructor(reload: () => void | Promise<void>) {
    this.root = panelShell('Models', this.body, reload);
    replaceChildren(this.body, [el('p', { class: 'muted', text: 'Loading…' })]);
  }

  render(providers: readonly ProviderView[], models: readonly ModelView[]): void {
    const nodes: HTMLElement[] = [];

    if (providers.length > 0) {
      const rows = providers.map((p) =>
        el(
          'tr',
          {},
          el('td', {}, el('code', { text: p.name })),
          el(
            'td',
            {},
            el('span', {
              class: `chip ${p.healthy ? 'chip-ok' : 'chip-error'}`,
              text: p.healthy ? 'healthy' : 'unavailable',
            }),
          ),
          el('td', { text: p.detail }),
        ),
      );
      nodes.push(
        el(
          'section',
          { class: 'stats-section' },
          el('h3', { class: 'panel-subtitle', text: 'Providers' }),
          el(
            'table',
            { class: 'table' },
            el('thead', {}, el('tr', {}, el('th', { text: 'Provider' }), el('th', { text: 'Health' }), el('th', { text: 'Detail' }))),
            el('tbody', {}, ...rows),
          ),
        ),
      );
    }

    if (models.length === 0) {
      nodes.push(el('p', { class: 'muted', text: 'No models are configured or reachable.' }));
    } else {
      const rows = models.map((m) =>
        el(
          'tr',
          {},
          el('td', {}, el('code', { text: m.id })),
          el('td', { text: m.provider }),
          el('td', { class: 'num', text: m.contextWindow > 0 ? formatNumber(m.contextWindow) : '—' }),
          el(
            'td',
            {},
            ...(m.capabilities.length > 0
              ? m.capabilities.map((c) => el('span', { class: 'chip', text: c }))
              : [el('span', { class: 'muted', text: 'unknown' })]),
          ),
        ),
      );
      nodes.push(
        el(
          'section',
          { class: 'stats-section' },
          el('h3', { class: 'panel-subtitle', text: 'Models' }),
          el(
            'table',
            { class: 'table' },
            el(
              'thead',
              {},
              el(
                'tr',
                {},
                el('th', { text: 'Model' }),
                el('th', { text: 'Provider' }),
                el('th', { class: 'num', text: 'Context' }),
                el('th', { text: 'Capabilities' }),
              ),
            ),
            el('tbody', {}, ...rows),
          ),
        ),
      );
    }
    replaceChildren(this.body, nodes);
  }

  setError(message: string): void {
    replaceChildren(this.body, [el('p', { class: 'notice notice-error', text: message })]);
  }
}

export class SessionsPanel {
  readonly root: HTMLElement;
  private readonly body = el('div', { class: 'panel-body' });

  constructor(reload: () => void | Promise<void>, onNew: () => void | Promise<void>) {
    this.root = panelShell(
      'Sessions',
      this.body,
      reload,
      el('button', { class: 'btn btn-tiny', type: 'button', text: 'New session', on: { click: () => void onNew() } }),
    );
    replaceChildren(this.body, [el('p', { class: 'muted', text: 'Loading…' })]);
  }

  render(sessions: readonly SessionView[], currentId: string): void {
    if (sessions.length === 0) {
      replaceChildren(this.body, [el('p', { class: 'muted', text: 'No sessions recorded yet.' })]);
      return;
    }
    const rows = sessions.map((s) =>
      el(
        'tr',
        { class: s.id === currentId ? 'is-current' : '' },
        el('td', { text: s.title }),
        el('td', {}, el('code', { text: s.model || '—' })),
        el('td', { text: s.provider }),
        el('td', { text: formatDateTime(s.updatedAt) }),
        el('td', {}, s.id === currentId ? el('span', { class: 'chip chip-ok', text: 'current' }) : null),
      ),
    );
    replaceChildren(this.body, [
      el(
        'table',
        { class: 'table' },
        el(
          'thead',
          {},
          el(
            'tr',
            {},
            el('th', { text: 'Title' }),
            el('th', { text: 'Model' }),
            el('th', { text: 'Provider' }),
            el('th', { text: 'Updated' }),
            el('th', { text: '' }),
          ),
        ),
        el('tbody', {}, ...rows),
      ),
    ]);
  }

  setError(message: string): void {
    replaceChildren(this.body, [el('p', { class: 'notice notice-error', text: message })]);
  }
}

export class SettingsPanel {
  readonly root: HTMLElement;
  private readonly body = el('div', { class: 'panel-body' });
  private readonly editor: HTMLTextAreaElement;
  private readonly status: HTMLElement;

  constructor(
    reload: () => void | Promise<void>,
    private readonly save: (config: unknown) => Promise<void>,
  ) {
    this.editor = el('textarea', {
      class: 'config-editor',
      attrs: { rows: '24', spellcheck: 'false' },
      aria: { label: 'Configuration (JSON)' },
    }) as HTMLTextAreaElement;
    this.status = el('p', { class: 'composer-hint', role: 'status', aria: { live: 'polite' } });
    this.root = panelShell('Settings', this.body, reload);
    replaceChildren(this.body, [
      el('p', {
        class: 'muted',
        text:
          'Configuration as the server reports it. Secrets are referenced by environment variable name, never by value — if you see a key here, that is a server bug.',
      }),
      this.editor,
      el(
        'div',
        { class: 'panel-actions' },
        el('button', {
          class: 'btn btn-primary',
          type: 'button',
          text: 'Save configuration',
          on: { click: () => void this.submit() },
        }),
      ),
      this.status,
    ]);
  }

  render(config: unknown): void {
    this.editor.value = JSON.stringify(config ?? {}, null, 2);
    this.status.textContent = '';
  }

  setError(message: string): void {
    this.status.textContent = message;
  }

  private async submit(): Promise<void> {
    let parsed: unknown;
    try {
      parsed = JSON.parse(this.editor.value) as unknown;
    } catch (err) {
      this.status.textContent = `Not valid JSON: ${err instanceof Error ? err.message : String(err)}`;
      return;
    }
    this.status.textContent = 'Saving…';
    try {
      await this.save(parsed);
      this.status.textContent = 'Saved.';
    } catch (err) {
      this.status.textContent = `Save failed: ${err instanceof Error ? err.message : String(err)}`;
    }
  }
}
