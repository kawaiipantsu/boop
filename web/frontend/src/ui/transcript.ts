// The conversation view.
//
// Performance contract: a streamed token must never cause the transcript to be
// rebuilt. Tokens are buffered and flushed once per animation frame into the
// text node of the block that is currently streaming, via appendData — so the
// cost of a token is O(token), not O(transcript).
//
// Accessibility contract: the transcript itself is not a live region. A token
// stream would make a screen reader unusable. Instead the finished assistant
// answer is announced once through a polite live region (approvals use an
// assertive one, in approvals.ts).
//
// Safety contract: model output and command output are untrusted. They reach
// the DOM only as text nodes.

import { el, isNearBottom, replaceChildren } from '../util/dom.js';
import { formatClock, formatDuration, truncate } from '../util/format.js';
import {
  chunkText, durationMs, errorText, isRecord, num, str, tokenText,
  type Action, type BoopEvent,
} from '../protocol.js';

/** Hard ceiling on retained characters per streamed output block. */
const MAX_STREAM_CHARS = 200_000;
/** How much to drop when the ceiling is hit, so trimming is amortised. */
const TRIM_CHARS = 50_000;

interface StreamBlock {
  node: Text;
  buffer: string;
  chars: number;
  trimmed: boolean;
  container: HTMLElement;
}

interface ToolEntry {
  row: HTMLElement;
  status: HTMLElement;
  meta: HTMLElement;
  startedAt: number;
}

interface CommandEntry {
  block: HTMLElement;
  status: HTMLElement;
  stdout: StreamBlock;
  stderr: StreamBlock;
  startedAt: number;
}

function trimNotice(container: HTMLElement): void {
  if (container.querySelector('.stream-trimmed')) return;
  container.insertBefore(
    el('p', {
      class: 'stream-trimmed',
      text: 'Earlier output was trimmed to keep this page responsive.',
    }),
    container.firstChild,
  );
}

export class Transcript {
  readonly root: HTMLElement;
  private readonly list: HTMLElement;
  private readonly announcer: HTMLElement;
  private readonly empty: HTMLElement;

  private assistant: StreamBlock | null = null;
  private reasoning: StreamBlock | null = null;
  private assistantText = '';
  private readonly pendingTools: Map<string, ToolEntry[]> = new Map();
  private readonly commands: CommandEntry[] = [];
  private readonly dirty = new Set<StreamBlock>();
  private frame: number | null = null;
  private lastUserText = '';
  private stickToBottom = true;

  constructor() {
    this.list = el('div', {
      class: 'transcript-list',
      role: 'log',
      aria: { label: 'Conversation transcript', live: 'off' },
    });
    this.empty = el(
      'div',
      { class: 'transcript-empty' },
      el('img', { class: 'transcript-mark', attrs: { src: 'boop-mark.svg', alt: '', width: '56', height: '56' } }),
      el('p', { text: 'Ask Boop something. Tool calls, command output and approvals all appear here.' }),
    );
    this.announcer = el('div', {
      class: 'sr-only',
      role: 'status',
      aria: { live: 'polite', atomic: 'true' },
    });
    this.root = el('div', { class: 'transcript', attrs: { tabindex: '0' } }, this.empty, this.list, this.announcer);
    this.root.addEventListener('scroll', () => {
      this.stickToBottom = isNearBottom(this.root);
    });
  }

  clear(): void {
    replaceChildren(this.list, []);
    this.assistant = null;
    this.reasoning = null;
    this.assistantText = '';
    this.pendingTools.clear();
    this.commands.length = 0;
    this.dirty.clear();
    this.lastUserText = '';
    this.empty.hidden = false;
  }

  /** Renders the user's turn immediately, before the server acknowledges it. */
  addUserTurn(text: string): void {
    this.lastUserText = text;
    this.closeAssistant();
    this.push(
      el(
        'article',
        { class: 'turn turn-user', aria: { label: 'You' } },
        el('div', { class: 'turn-role', text: 'You' }),
        el('div', { class: 'turn-body' }, el('p', { class: 'user-text', text })),
      ),
    );
  }

  addNotice(text: string, kind: 'info' | 'error' = 'info'): void {
    this.push(el('p', { class: `notice notice-${kind}`, role: kind === 'error' ? 'alert' : undefined, text }));
  }

  handle(ev: BoopEvent): void {
    switch (ev.type) {
      case 'session.started':
        break;
      case 'prompt.received': {
        const text = isRecord(ev.payload)
          ? str(ev.payload['text'] ?? ev.payload['prompt'] ?? ev.payload['content'])
          : str(ev.payload);
        // The composer already rendered this turn optimistically.
        if (text !== '' && text !== this.lastUserText) this.addUserTurn(text);
        this.lastUserText = '';
        break;
      }
      case 'model.request.started':
        this.closeReasoning();
        break;
      case 'model.token': {
        const text = tokenText(ev.payload);
        if (text !== '') this.appendAssistant(text);
        break;
      }
      case 'model.reasoning': {
        const text = tokenText(ev.payload);
        if (text !== '') this.appendReasoning(text);
        break;
      }
      case 'model.response.completed':
        this.flush();
        this.announceAnswer();
        this.closeAssistant();
        break;
      case 'tool.requested':
        this.toolRequested(ev.payload);
        break;
      case 'tool.completed':
        this.toolCompleted(ev.payload);
        break;
      case 'command.started':
        this.commandStarted(ev.payload);
        break;
      case 'command.stdout':
        this.commandChunk(chunkText(ev.payload), 'stdout');
        break;
      case 'command.stderr':
        this.commandChunk(chunkText(ev.payload), 'stderr');
        break;
      case 'command.completed':
        this.commandCompleted(ev.payload);
        break;
      case 'test.started':
        this.push(el('p', { class: 'notice notice-info', text: `Running tests${describeSuffix(ev.payload)}` }));
        break;
      case 'test.completed':
        this.testCompleted(ev.payload);
        break;
      case 'approval.requested':
        this.push(
          el('p', {
            class: 'notice notice-approval',
            text: 'Waiting for your approval — see the prompt above.',
          }),
        );
        break;
      case 'session.completed':
        this.flush();
        this.closeAssistant();
        break;
      case 'error':
        this.flush();
        this.closeAssistant();
        this.push(
          el(
            'div',
            { class: 'block block-error', role: 'alert' },
            el('div', { class: 'block-head' }, el('span', { class: 'block-title', text: 'Error' })),
            el('pre', { class: 'block-body' }, document.createTextNode(errorText(ev.payload))),
          ),
        );
        break;
      default:
        break;
    }
  }

  // -- streaming ------------------------------------------------------------

  private appendAssistant(text: string): void {
    if (!this.assistant) {
      const body = el('div', { class: 'turn-body' });
      const p = el('p', { class: 'assistant-text' });
      const node = document.createTextNode('');
      p.appendChild(node);
      body.appendChild(p);
      this.push(
        el(
          'article',
          { class: 'turn turn-assistant', aria: { label: 'Boop' } },
          el('div', { class: 'turn-role', text: 'Boop' }),
          body,
        ),
      );
      this.assistant = { node, buffer: '', chars: 0, trimmed: false, container: body };
      this.assistantText = '';
    }
    this.assistantText += text;
    this.queue(this.assistant, text);
  }

  private appendReasoning(text: string): void {
    if (!this.reasoning) {
      const pre = el('pre', { class: 'reasoning-body' });
      const node = document.createTextNode('');
      pre.appendChild(node);
      const details = el(
        'details',
        { class: 'block block-reasoning' },
        el('summary', { text: 'Reasoning' }),
        pre,
      );
      this.push(details);
      this.reasoning = { node, buffer: '', chars: 0, trimmed: false, container: details };
    }
    this.queue(this.reasoning, text);
  }

  private queue(block: StreamBlock, text: string): void {
    block.buffer += text;
    this.dirty.add(block);
    this.schedule();
  }

  private schedule(): void {
    if (this.frame !== null) return;
    const raf =
      typeof requestAnimationFrame === 'function'
        ? requestAnimationFrame
        : (fn: FrameRequestCallback): number => setTimeout(() => fn(0), 16) as unknown as number;
    this.frame = raf(() => {
      this.frame = null;
      this.flush();
    });
  }

  /** Applies every buffered chunk. Exposed for tests and for end-of-turn. */
  flush(): void {
    if (this.dirty.size === 0) return;
    for (const block of this.dirty) {
      if (block.buffer === '') continue;
      block.node.appendData(block.buffer);
      block.chars += block.buffer.length;
      block.buffer = '';
      if (block.chars > MAX_STREAM_CHARS) {
        block.node.deleteData(0, TRIM_CHARS);
        block.chars -= TRIM_CHARS;
        block.trimmed = true;
        trimNotice(block.container);
      }
    }
    this.dirty.clear();
    this.autoscroll();
  }

  private announceAnswer(): void {
    const text = this.assistantText.trim();
    if (text === '') return;
    this.announcer.textContent = `Boop replied: ${truncate(text, 600)}`;
  }

  private closeAssistant(): void {
    this.flush();
    this.assistant = null;
    this.closeReasoning();
  }

  private closeReasoning(): void {
    this.reasoning = null;
  }

  // -- tools ----------------------------------------------------------------

  private toolRequested(payload: unknown): void {
    const action = payload as Partial<Action> | undefined;
    const rec = isRecord(payload) ? payload : {};
    const tool = str(rec['tool'], str(action?.tool, 'tool'));
    const summary = str(rec['summary']);
    const detail = str(rec['detail']);
    const risk = str(rec['risk']);
    const production = rec['production'] === true;

    const status = el('span', { class: 'chip chip-running', text: 'running' });
    const meta = el('span', { class: 'block-meta' });
    const head = el(
      'div',
      { class: 'block-head' },
      el('span', { class: 'block-title', text: tool }),
      status,
      risk ? el('span', { class: `chip chip-risk chip-risk-${risk}`, text: risk }) : null,
      production ? el('span', { class: 'chip chip-prod', text: 'production' }) : null,
      meta,
    );
    const row = el('div', { class: 'block block-tool', data: { tool } }, head);
    if (summary !== '') row.appendChild(el('p', { class: 'block-summary', text: summary }));
    if (detail !== '') row.appendChild(el('pre', { class: 'block-body' }, document.createTextNode(detail)));
    this.push(row);

    const list = this.pendingTools.get(tool) ?? [];
    list.push({ row, status, meta, startedAt: Date.now() });
    this.pendingTools.set(tool, list);
  }

  private toolCompleted(payload: unknown): void {
    const rec = isRecord(payload) ? payload : {};
    const tool = str(rec['tool']);
    const failed = rec['error'] === true;
    const ms = durationMs(rec['duration'] ?? rec['duration_ms']);

    const list = this.pendingTools.get(tool);
    const entry = list?.shift();
    if (!entry) {
      this.push(
        el(
          'div',
          { class: 'block block-tool' },
          el(
            'div',
            { class: 'block-head' },
            el('span', { class: 'block-title', text: tool || 'tool' }),
            el('span', { class: `chip ${failed ? 'chip-error' : 'chip-ok'}`, text: failed ? 'failed' : 'ok' }),
          ),
        ),
      );
      return;
    }
    if (list && list.length === 0) this.pendingTools.delete(tool);

    entry.status.className = `chip ${failed ? 'chip-error' : 'chip-ok'}`;
    entry.status.textContent = failed ? 'failed' : 'ok';
    entry.row.classList.add(failed ? 'is-error' : 'is-ok');
    const elapsed = ms ?? Date.now() - entry.startedAt;
    entry.meta.textContent = formatDuration(elapsed);
    this.autoscroll();
  }

  // -- commands -------------------------------------------------------------

  private commandStarted(payload: unknown): void {
    const rec = isRecord(payload) ? payload : {};
    const command = str(rec['command'], str(payload));
    const cwd = str(rec['working_dir'] ?? rec['cwd']);
    const status = el('span', { class: 'chip chip-running', text: 'running' });

    const stdoutPre = el('pre', { class: 'stream stream-stdout' });
    const stdoutNode = document.createTextNode('');
    stdoutPre.appendChild(stdoutNode);
    const stderrPre = el('pre', { class: 'stream stream-stderr', hidden: true });
    const stderrNode = document.createTextNode('');
    stderrPre.appendChild(stderrNode);

    const block = el(
      'div',
      { class: 'block block-command' },
      el(
        'div',
        { class: 'block-head' },
        el('span', { class: 'block-title', text: '$' }),
        el('code', { class: 'command-line', text: command || '(command)' }),
        status,
      ),
      cwd !== '' ? el('p', { class: 'block-meta', text: cwd }) : null,
      stdoutPre,
      stderrPre,
    );
    this.push(block);
    this.commands.push({
      block,
      status,
      stdout: { node: stdoutNode, buffer: '', chars: 0, trimmed: false, container: block },
      stderr: { node: stderrNode, buffer: '', chars: 0, trimmed: false, container: block },
      startedAt: Date.now(),
    });
  }

  private activeCommand(): CommandEntry | null {
    for (let i = this.commands.length - 1; i >= 0; i -= 1) {
      const c = this.commands[i] as CommandEntry;
      if (!c.block.classList.contains('is-done')) return c;
    }
    return null;
  }

  private commandChunk(text: string, stream: 'stdout' | 'stderr'): void {
    if (text === '') return;
    let entry = this.activeCommand();
    if (!entry) {
      this.commandStarted({ command: '' });
      entry = this.activeCommand();
      if (!entry) return;
    }
    const block = stream === 'stdout' ? entry.stdout : entry.stderr;
    if (stream === 'stderr') {
      const pre = entry.block.querySelector('.stream-stderr');
      if (pre instanceof HTMLElement) pre.hidden = false;
    }
    this.queue(block, text.endsWith('\n') ? text : `${text}\n`);
  }

  private commandCompleted(payload: unknown): void {
    const entry = this.activeCommand();
    const rec = isRecord(payload) ? payload : {};
    const exit = num(rec['exit_code'], -1);
    const timedOut = rec['timed_out'] === true;
    const cancelled = rec['cancelled'] === true;
    const ms = durationMs(rec['duration'] ?? rec['duration_ms']);
    this.flush();
    if (!entry) return;

    const ok = exit === 0 && !timedOut && !cancelled;
    entry.block.classList.add('is-done', ok ? 'is-ok' : 'is-error');
    entry.status.className = `chip ${ok ? 'chip-ok' : 'chip-error'}`;
    entry.status.textContent = timedOut
      ? 'timed out'
      : cancelled
        ? 'cancelled'
        : ok
          ? 'exit 0'
          : `exit ${exit}`;
    const elapsed = ms ?? Date.now() - entry.startedAt;
    const meta = formatDuration(elapsed);
    if (meta !== '') entry.block.appendChild(el('p', { class: 'block-meta', text: meta }));
    this.autoscroll();
  }

  private testCompleted(payload: unknown): void {
    const rec = isRecord(payload) ? payload : {};
    const passed = num(rec['passed']);
    const failed = num(rec['failed']);
    const skipped = num(rec['skipped']);
    const ok = failed === 0;
    this.push(
      el('p', {
        class: `notice ${ok ? 'notice-ok' : 'notice-error'}`,
        text: `Tests finished — ${passed} passed, ${failed} failed, ${skipped} skipped.`,
      }),
    );
  }

  // -- plumbing -------------------------------------------------------------

  private push(node: HTMLElement): void {
    this.empty.hidden = true;
    const stamp = el('time', { class: 'turn-time', text: formatClock(new Date().toISOString()) });
    node.appendChild(stamp);
    this.list.appendChild(node);
    this.autoscroll();
  }

  private autoscroll(): void {
    if (!this.stickToBottom) return;
    this.root.scrollTop = this.root.scrollHeight;
  }
}

function describeSuffix(payload: unknown): string {
  const rec = isRecord(payload) ? payload : {};
  const target = str(rec['command'] ?? rec['suite'] ?? rec['package']);
  return target === '' ? '…' : `: ${truncate(target, 80)}`;
}
