// Approval prompt (PROJECT.md §14, §49, §50).
//
// Rules this file exists to enforce:
//   * A WebUI approval is never less explicit than the TUI one. Summary, the
//     literal detail (command line or path), category, risk and the evaluator's
//     reason are all shown — no truncation of the detail.
//   * A production-affecting action is visually distinct and cannot be granted
//     for the rest of the session; it is approve-once or reject.
//   * It is unmissable and modal: focus moves into it, Tab is trapped, and it
//     cannot be dismissed without an answer.
//   * It is announced assertively, because unlike streamed tokens this does
//     require interrupting the user.

import { el, replaceChildren, trapFocus } from '../util/dom.js';
import { formatClock } from '../util/format.js';
import type { Approval, GrantScope, Risk } from '../protocol.js';

export interface ApprovalHandlers {
  resolve: (id: string, approved: boolean, scope: GrantScope) => void | Promise<void>;
}

const RISK_BLURB: Record<Risk, string> = {
  low: 'Routine action.',
  medium: 'This changes something on this machine.',
  high: 'This can destroy work or affect systems outside this machine.',
  critical: 'This is irreversible or destructive. Read it carefully.',
};

/** Mirrors permissions.Broker: dangerous actions are approve-once only. */
export function isGrantable(approval: Approval): boolean {
  const { risk, production, category } = approval.action;
  return risk !== 'critical' && !production && category !== 'production.change';
}

export function isSevere(approval: Approval): boolean {
  const { risk, production, category } = approval.action;
  return risk === 'high' || risk === 'critical' || production || category === 'production.change';
}

export class ApprovalPanel {
  readonly root: HTMLElement;
  private readonly announcer: HTMLElement;
  private readonly handlers: ApprovalHandlers;
  private releaseTrap: (() => void) | null = null;
  private returnFocus: HTMLElement | null = null;
  private shownId = '';
  private busy = false;

  constructor(handlers: ApprovalHandlers) {
    this.handlers = handlers;
    this.root = el('div', {
      class: 'approvals',
      role: 'region',
      aria: { label: 'Pending approval' },
      hidden: true,
    });
    this.announcer = el('div', { class: 'sr-only', role: 'alert', aria: { live: 'assertive', atomic: 'true' } });
    this.root.appendChild(this.announcer);
  }

  /** Renders the head of the queue. Called on every state change. */
  render(queue: readonly Approval[]): void {
    const next = queue[0];
    if (!next) {
      if (this.shownId !== '') this.dismiss();
      return;
    }
    if (next.id === this.shownId) {
      this.updateBacklog(queue.length - 1);
      return;
    }
    this.show(next, queue.length - 1);
  }

  private dismiss(): void {
    this.shownId = '';
    this.busy = false;
    this.releaseTrap?.();
    this.releaseTrap = null;
    this.root.hidden = true;
    replaceChildren(this.root, [this.announcer]);
    const target = this.returnFocus;
    this.returnFocus = null;
    if (target && typeof target.focus === 'function' && target.isConnected) target.focus();
  }

  private updateBacklog(remaining: number): void {
    const node = this.root.querySelector('.approval-backlog');
    if (!(node instanceof HTMLElement)) return;
    node.hidden = remaining <= 0;
    node.textContent = remaining === 1 ? '1 more approval waiting' : `${remaining} more approvals waiting`;
  }

  private show(approval: Approval, remaining: number): void {
    const severe = isSevere(approval);
    const grantable = isGrantable(approval);
    const { action } = approval;
    const active = document.activeElement;
    if (!this.returnFocus && active instanceof HTMLElement && !this.root.contains(active)) {
      this.returnFocus = active;
    }

    this.shownId = approval.id;
    this.busy = false;

    const heading = el('h2', {
      class: 'approval-title',
      id: 'approval-title',
      text: action.summary !== '' ? action.summary : `Boop wants to use ${action.tool || 'a tool'}`,
    });

    const chips = el(
      'div',
      { class: 'approval-chips' },
      el('span', { class: `chip chip-risk chip-risk-${action.risk}`, text: `risk: ${action.risk}` }),
      action.category !== '' ? el('span', { class: 'chip', text: action.category }) : null,
      action.tool !== '' ? el('span', { class: 'chip', text: action.tool }) : null,
      action.production ? el('span', { class: 'chip chip-prod', text: 'PRODUCTION' }) : null,
    );

    const facts = el('dl', { class: 'approval-facts' });
    if (action.detail !== '') {
      facts.appendChild(el('dt', { text: 'Action' }));
      // Untrusted: a text node inside <pre>, never markup.
      facts.appendChild(el('dd', {}, el('pre', { class: 'approval-detail' }, document.createTextNode(action.detail))));
    }
    if (action.paths.length > 0) {
      facts.appendChild(el('dt', { text: action.paths.length === 1 ? 'Path' : 'Paths' }));
      const list = el('ul', { class: 'approval-paths' });
      for (const p of action.paths) list.appendChild(el('li', {}, el('code', { text: p })));
      facts.appendChild(el('dd', {}, list));
    }
    if (approval.reason !== '') {
      facts.appendChild(el('dt', { text: 'Why you are being asked' }));
      facts.appendChild(el('dd', { text: approval.reason }));
    }
    facts.appendChild(el('dt', { text: 'Risk' }));
    facts.appendChild(
      el('dd', {
        text: `${action.risk.toUpperCase()} — ${RISK_BLURB[action.risk]}${
          action.production ? ' This action can affect production.' : ''
        }`,
      }),
    );

    const buttons = el('div', { class: 'approval-actions' });
    const approve = el('button', {
      class: `btn btn-approve${severe ? ' btn-severe' : ''}`,
      type: 'button',
      text: severe ? 'Approve once' : 'Approve',
      on: { click: () => void this.answer(approval.id, true, 'once') },
    });
    const always = grantable
      ? el('button', {
          class: 'btn btn-secondary',
          type: 'button',
          text: 'Always for this session',
          title: `Allow every later ${action.category || 'action of this kind'} for the rest of this session`,
          on: { click: () => void this.answer(approval.id, true, 'session.category') },
        })
      : null;
    const reject = el('button', {
      class: 'btn btn-reject',
      type: 'button',
      text: 'Reject',
      on: { click: () => void this.answer(approval.id, false, 'once') },
    });
    buttons.appendChild(approve);
    if (always) buttons.appendChild(always);
    buttons.appendChild(reject);

    const backlog = el('p', { class: 'approval-backlog', hidden: remaining <= 0 });

    const dialog = el(
      'div',
      {
        class: `approval${severe ? ' approval-severe' : ''}`,
        role: 'dialog',
        aria: { modal: 'true', labelledby: 'approval-title', describedby: 'approval-desc' },
      },
      el(
        'div',
        { class: 'approval-head' },
        el('span', { class: 'approval-kicker', text: severe ? 'Approval required — high impact' : 'Approval required' }),
        approval.requestedAt !== ''
          ? el('time', { class: 'approval-time', text: formatClock(approval.requestedAt) })
          : null,
      ),
      heading,
      chips,
      el('div', { class: 'approval-desc', id: 'approval-desc' }, facts),
      buttons,
      backlog,
      el('p', {
        class: 'approval-hint',
        text: 'Boop is blocked until you answer. Nothing runs without your decision.',
      }),
    );

    replaceChildren(this.root, [this.announcer, dialog]);
    this.root.hidden = false;
    this.updateBacklog(remaining);

    this.releaseTrap?.();
    this.releaseTrap = trapFocus(dialog);
    dialog.addEventListener('keydown', (ev: Event) => {
      const key = (ev as KeyboardEvent).key;
      // Escape must not dismiss an approval; it steers to the safe answer.
      if (key === 'Escape') {
        ev.preventDefault();
        reject.focus();
      }
    });

    // Focus lands on the safe answer for anything severe.
    (severe ? reject : approve).focus();

    this.announcer.textContent = `Approval required. ${
      action.summary || action.tool
    }. Risk ${action.risk}.${action.production ? ' This affects production.' : ''} ${
      action.detail !== '' ? action.detail : ''
    }`;
  }

  private async answer(id: string, approved: boolean, scope: GrantScope): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    for (const b of Array.from(this.root.querySelectorAll('button'))) b.disabled = true;
    try {
      await this.handlers.resolve(id, approved, scope);
    } finally {
      this.busy = false;
    }
  }
}
