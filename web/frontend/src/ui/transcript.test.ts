import assert from 'node:assert/strict';
import { afterEach, beforeEach, describe, it } from 'node:test';

import { setupDom } from '../testing/dom.js';
import { Transcript } from './transcript.js';

let dom: ReturnType<typeof setupDom>;
let t: Transcript;

beforeEach(() => {
  dom = setupDom();
  t = new Transcript();
  document.body.appendChild(t.root);
});
afterEach(() => dom.cleanup());

function assistantText(): string {
  return t.root.querySelector('.assistant-text')?.textContent ?? '';
}

describe('Transcript streaming', () => {
  it('appends tokens into one text node instead of re-rendering', () => {
    for (const token of ['Hel', 'lo, ', 'world']) {
      t.handle({ type: 'model.token', payload: token });
    }
    t.flush();
    const paragraphs = t.root.querySelectorAll('.assistant-text');
    assert.equal(paragraphs.length, 1);
    const p = paragraphs[0] as Element;
    // One text node, mutated in place: that is the performance contract.
    assert.equal(p.childNodes.length, 1);
    assert.equal(p.firstChild?.nodeType, 3);
    assert.equal(assistantText(), 'Hello, world');
  });

  it('handles an object token payload as well as a bare string', () => {
    t.handle({ type: 'model.token', payload: { text: 'a' } });
    t.handle({ type: 'model.token', payload: 'b' });
    t.flush();
    assert.equal(assistantText(), 'ab');
  });

  it('starts a new assistant block after each completed response', () => {
    t.handle({ type: 'model.token', payload: 'one' });
    t.handle({ type: 'model.response.completed', payload: {} });
    t.handle({ type: 'model.token', payload: 'two' });
    t.flush();
    assert.equal(t.root.querySelectorAll('.assistant-text').length, 2);
  });

  it('never interprets model output as markup', () => {
    t.handle({ type: 'model.token', payload: '<img src=x onerror="alert(1)"><script>bad()</script>' });
    t.flush();
    const list = t.root.querySelector('.transcript-list') as HTMLElement;
    assert.equal(list.querySelectorAll('img').length, 0);
    assert.equal(list.querySelectorAll('script').length, 0);
    assert.match(assistantText(), /<img src=x/);
  });

  it('announces the finished answer politely, not every token', () => {
    const live = t.root.querySelector('[aria-live="polite"]') as HTMLElement;
    assert.ok(live);
    t.handle({ type: 'model.token', payload: 'partial' });
    t.flush();
    assert.equal(live.textContent, '');
    t.handle({ type: 'model.response.completed', payload: {} });
    assert.match(live.textContent ?? '', /Boop replied: partial/);
  });

  it('keeps the transcript itself out of the live region', () => {
    const log = t.root.querySelector('[role="log"]') as HTMLElement;
    assert.equal(log.getAttribute('aria-live'), 'off');
  });

  it('puts reasoning in a collapsed details block, separate from the answer', () => {
    t.handle({ type: 'model.reasoning', payload: 'thinking…' });
    t.handle({ type: 'model.token', payload: 'answer' });
    t.flush();
    const details = t.root.querySelector('details.block-reasoning');
    assert.ok(details);
    assert.match(details.textContent ?? '', /thinking/);
    assert.equal(assistantText(), 'answer');
  });
});

describe('Transcript tool calls', () => {
  it('shows a running tool and then its outcome and duration', () => {
    t.handle({
      type: 'tool.requested',
      payload: { tool: 'run', summary: 'Run a command', detail: 'go test ./...', risk: 'low', category: 'shell.execute' },
    });
    const block = t.root.querySelector('.block-tool') as HTMLElement;
    assert.ok(block);
    assert.match(block.textContent ?? '', /running/);
    assert.match(block.textContent ?? '', /go test \.\/\.\.\./);

    t.handle({ type: 'tool.completed', payload: { tool: 'run', error: false, duration: '1.5s' } });
    assert.match(block.textContent ?? '', /ok/);
    assert.match(block.textContent ?? '', /1\.50s/);
    assert.ok(block.classList.contains('is-ok'));
  });

  it('marks a failed tool', () => {
    t.handle({ type: 'tool.requested', payload: { tool: 'write', detail: '/etc/hosts' } });
    t.handle({ type: 'tool.completed', payload: { tool: 'write', error: true, duration: 2_000_000 } });
    const block = t.root.querySelector('.block-tool') as HTMLElement;
    assert.ok(block.classList.contains('is-error'));
    assert.match(block.textContent ?? '', /failed/);
  });

  it('matches concurrent calls to the same tool in order', () => {
    t.handle({ type: 'tool.requested', payload: { tool: 'run', detail: 'first' } });
    t.handle({ type: 'tool.requested', payload: { tool: 'run', detail: 'second' } });
    t.handle({ type: 'tool.completed', payload: { tool: 'run', error: false } });
    const blocks = Array.from(t.root.querySelectorAll('.block-tool'));
    assert.equal(blocks[0]?.classList.contains('is-ok'), true);
    assert.equal(blocks[1]?.classList.contains('is-ok'), false);
  });

  it('does not crash on a completion with no matching request', () => {
    t.handle({ type: 'tool.completed', payload: { tool: 'ghost', error: false } });
    assert.match(t.root.textContent ?? '', /ghost/);
  });
});

describe('Transcript command output', () => {
  it('streams stdout and stderr into the active command block', () => {
    t.handle({ type: 'command.started', payload: { command: 'go build ./...', working_dir: '/repo' } });
    t.handle({ type: 'command.stdout', payload: 'compiling\n' });
    t.handle({ type: 'command.stderr', payload: { chunk: 'vet: warning' } });
    t.flush();
    const stdout = t.root.querySelector('.stream-stdout') as HTMLElement;
    const stderr = t.root.querySelector('.stream-stderr') as HTMLElement;
    assert.equal(stdout.textContent, 'compiling\n');
    assert.equal(stderr.textContent, 'vet: warning\n');
    assert.equal(stderr.hidden, false);
  });

  it('reports the exit status', () => {
    t.handle({ type: 'command.started', payload: { command: 'false' } });
    t.handle({ type: 'command.completed', payload: { exit_code: 1, duration: '12ms' } });
    const block = t.root.querySelector('.block-command') as HTMLElement;
    assert.match(block.textContent ?? '', /exit 1/);
    assert.ok(block.classList.contains('is-error'));
  });

  it('distinguishes a timeout from a plain non-zero exit', () => {
    t.handle({ type: 'command.started', payload: { command: 'sleep 100' } });
    t.handle({ type: 'command.completed', payload: { exit_code: -1, timed_out: true } });
    assert.match(t.root.textContent ?? '', /timed out/);
  });

  it('treats command output as text, never markup', () => {
    t.handle({ type: 'command.started', payload: { command: 'cat evil.html' } });
    t.handle({ type: 'command.stdout', payload: '<script>alert(1)</script>' });
    t.flush();
    assert.equal(t.root.querySelectorAll('script').length, 0);
    assert.match(t.root.querySelector('.stream-stdout')?.textContent ?? '', /<script>/);
  });

  it('creates a block for orphan output rather than dropping it', () => {
    t.handle({ type: 'command.stdout', payload: 'output with no start event' });
    t.flush();
    assert.match(t.root.textContent ?? '', /output with no start event/);
  });

  it('trims a runaway stream and says so', () => {
    t.handle({ type: 'command.started', payload: { command: 'yes' } });
    for (let i = 0; i < 30; i += 1) {
      t.handle({ type: 'command.stdout', payload: 'x'.repeat(10_000) });
      t.flush();
    }
    const stdout = t.root.querySelector('.stream-stdout') as HTMLElement;
    assert.ok((stdout.textContent ?? '').length <= 250_000);
    assert.match(t.root.textContent ?? '', /trimmed/);
  });
});

describe('Transcript turns and errors', () => {
  it('renders the user turn once even though prompt.received echoes it', () => {
    t.addUserTurn('hello');
    t.handle({ type: 'prompt.received', payload: { text: 'hello' } });
    assert.equal(t.root.querySelectorAll('.turn-user').length, 1);
  });

  it('renders a user turn that arrived from another client', () => {
    t.handle({ type: 'prompt.received', payload: { text: 'from the TUI' } });
    assert.equal(t.root.querySelectorAll('.turn-user').length, 1);
    assert.match(t.root.textContent ?? '', /from the TUI/);
  });

  it('renders errors as an alert', () => {
    t.handle({ type: 'error', payload: { message: 'provider unreachable' } });
    const block = t.root.querySelector('.block-error') as HTMLElement;
    assert.equal(block.getAttribute('role'), 'alert');
    assert.match(block.textContent ?? '', /provider unreachable/);
  });

  it('summarises test results', () => {
    t.handle({ type: 'test.started', payload: { command: 'go test ./...' } });
    t.handle({ type: 'test.completed', payload: { passed: 12, failed: 1, skipped: 0 } });
    assert.match(t.root.textContent ?? '', /12 passed, 1 failed/);
  });

  it('clear() empties the conversation and the empty state returns', () => {
    t.addUserTurn('hi');
    t.clear();
    assert.equal(t.root.querySelectorAll('.turn').length, 0);
    assert.equal((t.root.querySelector('.transcript-empty') as HTMLElement).hidden, false);
  });
});
