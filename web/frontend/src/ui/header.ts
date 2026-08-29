// Header: what Boop is doing right now, and whether we can still see it.

import { el } from '../util/dom.js';
import { formatCompact, formatDuration, titleCase, truncate } from '../util/format.js';
import type { AppState } from '../store.js';

interface Field {
  root: HTMLElement;
  value: HTMLElement;
}

function field(label: string, extraClass = ''): Field {
  const value = el('span', { class: 'field-value', text: '—' });
  const root = el(
    'div',
    { class: `field ${extraClass}`.trim() },
    el('span', { class: 'field-label', text: label }),
    value,
  );
  return { root, value };
}

const CONNECTION_LABEL: Record<AppState['connection'], string> = {
  connecting: 'Connecting…',
  open: 'Live',
  reconnecting: 'Reconnecting…',
  offline: 'Disconnected',
};

export class Header {
  readonly root: HTMLElement;
  private readonly provider = field('Provider');
  private readonly model = field('Model');
  private readonly mode = field('Mode');
  private readonly tokens = field('Tokens');
  private readonly agents = field('Agents');
  private readonly conn: HTMLElement;
  private readonly connDot: HTMLElement;
  private readonly connText: HTMLElement;
  private readonly project: HTMLElement;
  private readonly retryButton: HTMLButtonElement;

  constructor(onRetry: () => void) {
    this.connDot = el('span', { class: 'conn-dot', aria: { hidden: 'true' } });
    this.connText = el('span', { class: 'conn-text', text: 'Connecting…' });
    this.retryButton = el('button', {
      class: 'btn btn-tiny',
      type: 'button',
      text: 'Retry now',
      hidden: true,
      on: { click: onRetry },
    });
    this.conn = el(
      'div',
      { class: 'conn', role: 'status', aria: { live: 'polite' } },
      this.connDot,
      this.connText,
      this.retryButton,
    );
    this.project = el('span', { class: 'brand-project', text: '' });

    this.root = el(
      'header',
      { class: 'header' },
      el(
        'div',
        { class: 'brand' },
        el('img', {
          class: 'brand-mark',
          attrs: { src: 'boop-mark.svg', alt: 'Boop', width: '28', height: '28' },
        }),
        el('span', { class: 'brand-name', text: 'boop' }),
        this.project,
      ),
      el(
        'div',
        { class: 'fields' },
        this.provider.root,
        this.model.root,
        this.mode.root,
        this.tokens.root,
        this.agents.root,
      ),
      this.conn,
    );
  }

  update(state: AppState): void {
    const s = state.status;
    this.provider.value.textContent = s.provider || '—';
    this.model.value.textContent = s.model || '—';
    this.mode.value.textContent = s.mode ? titleCase(s.mode) : '—';
    this.mode.root.classList.toggle('is-auto', s.mode === 'auto');
    this.tokens.value.textContent = s.tokens.total > 0 ? formatCompact(s.tokens.total) : '0';
    this.tokens.root.title =
      `${s.tokens.prompt} prompt + ${s.tokens.completion} completion = ${s.tokens.total} tokens`;
    this.agents.value.textContent = String(state.agents.length || s.agents || 0);
    this.project.textContent = s.projectPath ? truncate(s.projectPath, 48) : '';
    this.project.title = s.projectPath;

    this.conn.dataset['state'] = state.connection;
    const label = CONNECTION_LABEL[state.connection];
    const retryIn =
      state.connection !== 'open' && state.nextRetryMs > 0
        ? ` retrying in ${formatDuration(state.nextRetryMs)}`
        : '';
    this.connText.textContent = `${label}${retryIn}`;
    this.retryButton.hidden = state.connection === 'open' || state.connection === 'connecting';
    this.connDot.className = `conn-dot conn-dot-${state.connection}`;
  }
}
