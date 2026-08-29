import assert from 'node:assert/strict';
import { afterEach, beforeEach, describe, it } from 'node:test';

import { setupDom } from '../testing/dom.js';
import { ApprovalPanel, isGrantable, isSevere } from './approvals.js';
import type { Approval, GrantScope, Risk } from '../protocol.js';

function approval(over: Partial<Approval['action']> & { id?: string; reason?: string } = {}): Approval {
  const { id, reason, ...action } = over;
  return {
    id: id ?? 'a1',
    reason: reason ?? 'shell.execute requires confirmation in confirm mode',
    requestedAt: '2026-01-01T10:00:00Z',
    action: {
      category: 'shell.execute',
      risk: 'low',
      tool: 'run',
      summary: 'Boop wants to run a command',
      detail: 'go test ./...',
      paths: [],
      production: false,
      ...action,
    },
  };
}

let dom: ReturnType<typeof setupDom>;
let calls: Array<[string, boolean, GrantScope]>;
let panel: ApprovalPanel;

beforeEach(() => {
  dom = setupDom();
  calls = [];
  panel = new ApprovalPanel({
    resolve: (id, approved, scope) => {
      calls.push([id, approved, scope]);
    },
  });
  document.body.appendChild(panel.root);
});
afterEach(() => dom.cleanup());

function buttons(): HTMLButtonElement[] {
  return Array.from(panel.root.querySelectorAll('button'));
}
function labelled(text: string): HTMLButtonElement {
  const found = buttons().find((b) => b.textContent === text);
  assert.ok(found, `no button labelled ${text}; got ${buttons().map((b) => b.textContent).join(', ')}`);
  return found;
}

describe('approval classification', () => {
  it('treats critical, production-flagged and production-category actions as severe', () => {
    assert.equal(isSevere(approval({ risk: 'low' })), false);
    assert.equal(isSevere(approval({ risk: 'high' })), true);
    assert.equal(isSevere(approval({ risk: 'critical' })), true);
    assert.equal(isSevere(approval({ risk: 'low', production: true })), true);
    assert.equal(isSevere(approval({ risk: 'low', category: 'production.change' })), true);
  });

  it('refuses a session-wide grant for anything the broker would refuse', () => {
    assert.equal(isGrantable(approval({ risk: 'medium' })), true);
    assert.equal(isGrantable(approval({ risk: 'high' })), true);
    assert.equal(isGrantable(approval({ risk: 'critical' })), false);
    assert.equal(isGrantable(approval({ production: true })), false);
    assert.equal(isGrantable(approval({ category: 'production.change' })), false);
  });
});

describe('ApprovalPanel', () => {
  it('is hidden until something needs approving', () => {
    panel.render([]);
    assert.equal(panel.root.hidden, true);
  });

  it('shows summary, verbatim detail, category, risk and reason (§50 parity with the TUI)', () => {
    panel.render([approval({ paths: ['/repo/main.go'] })]);
    const text = panel.root.textContent ?? '';
    assert.equal(panel.root.hidden, false);
    assert.match(text, /Boop wants to run a command/);
    assert.match(text, /go test \.\/\.\.\./);
    assert.match(text, /shell\.execute/);
    assert.match(text, /risk: low/);
    assert.match(text, /LOW/);
    assert.match(text, /requires confirmation/);
    assert.match(text, /\/repo\/main\.go/);
  });

  it('does not truncate a long command', () => {
    const long = `echo ${'a'.repeat(400)}`;
    panel.render([approval({ detail: long })]);
    assert.equal(panel.root.querySelector('.approval-detail')?.textContent, long);
  });

  it('renders a detail that looks like markup as text', () => {
    panel.render([approval({ detail: '<script>alert(1)</script>' })]);
    assert.equal(panel.root.querySelectorAll('script').length, 0);
    assert.match(panel.root.querySelector('.approval-detail')?.textContent ?? '', /<script>/);
  });

  it('is a modal dialog with an assertive announcement', () => {
    panel.render([approval()]);
    const dialog = panel.root.querySelector('[role="dialog"]') as HTMLElement;
    assert.equal(dialog.getAttribute('aria-modal'), 'true');
    const alert = panel.root.querySelector('[aria-live="assertive"]') as HTMLElement;
    assert.match(alert.textContent ?? '', /Approval required/);
    assert.match(alert.textContent ?? '', /go test/);
  });

  it('offers approve, always-for-session and reject for an ordinary action', () => {
    panel.render([approval()]);
    assert.deepEqual(buttons().map((b) => b.textContent), ['Approve', 'Always for this session', 'Reject']);
  });

  it('offers approve-once and reject only for a production action, and marks it distinctly', () => {
    panel.render([approval({ risk: 'high', production: true, category: 'production.change' })]);
    assert.deepEqual(buttons().map((b) => b.textContent), ['Approve once', 'Reject']);
    const dialog = panel.root.querySelector('.approval') as HTMLElement;
    assert.ok(dialog.classList.contains('approval-severe'));
    assert.match(panel.root.textContent ?? '', /PRODUCTION/);
    assert.match(panel.root.textContent ?? '', /can affect production/);
  });

  it('focuses approve for a low-risk action and reject for a severe one', () => {
    panel.render([approval()]);
    assert.equal(document.activeElement?.textContent, 'Approve');
    panel.render([]);
    panel.render([approval({ id: 'a2', risk: 'critical' })]);
    assert.equal(document.activeElement?.textContent, 'Reject');
  });

  it('sends the chosen decision and scope', () => {
    panel.render([approval()]);
    labelled('Always for this session').click();
    assert.deepEqual(calls, [['a1', true, 'session.category']]);

    calls.length = 0;
    panel.render([]);
    panel.render([approval({ id: 'a2' })]);
    labelled('Reject').click();
    assert.deepEqual(calls, [['a2', false, 'once']]);
  });

  it('ignores a second click while the first decision is in flight', () => {
    panel.render([approval()]);
    const approve = labelled('Approve');
    approve.click();
    approve.click();
    assert.equal(calls.length, 1);
  });

  it('Escape steers to Reject instead of dismissing the prompt', () => {
    panel.render([approval()]);
    const dialog = panel.root.querySelector('[role="dialog"]') as HTMLElement;
    dialog.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    assert.equal(panel.root.hidden, false);
    assert.equal(document.activeElement?.textContent, 'Reject');
  });

  it('traps Tab inside the dialog', () => {
    panel.render([approval()]);
    const dialog = panel.root.querySelector('[role="dialog"]') as HTMLElement;
    const all = buttons();
    (all[all.length - 1] as HTMLButtonElement).focus();
    dialog.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Tab', bubbles: true }));
    assert.equal(document.activeElement, all[0]);
  });

  it('reports how many more are waiting and moves to the next when one clears', () => {
    const a = approval({ id: 'a1' });
    const b = approval({ id: 'b1', summary: 'Second thing', detail: 'rm -rf build' });
    panel.render([a, b]);
    assert.match(panel.root.textContent ?? '', /1 more approval waiting/);
    panel.render([b]);
    assert.match(panel.root.textContent ?? '', /Second thing/);
    assert.equal((panel.root.querySelector('.approval-backlog') as HTMLElement).hidden, true);
  });

  it('restores focus to whatever had it once the queue empties', () => {
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    panel.render([approval()]);
    assert.notEqual(document.activeElement, input);
    panel.render([]);
    assert.equal(document.activeElement, input);
    assert.equal(panel.root.hidden, true);
  });

  it('does not rebuild the dialog while the same approval is showing', () => {
    panel.render([approval()]);
    const dialog = panel.root.querySelector('[role="dialog"]');
    panel.render([approval(), approval({ id: 'b1' })]);
    assert.equal(panel.root.querySelector('[role="dialog"]'), dialog);
  });

  it('describes every risk level', () => {
    for (const risk of ['low', 'medium', 'high', 'critical'] as Risk[]) {
      panel.render([]);
      panel.render([approval({ id: `r-${risk}`, risk })]);
      assert.match(panel.root.textContent ?? '', new RegExp(risk.toUpperCase()));
    }
  });
});
