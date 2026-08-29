// Prompt input and the interrupt control (§26, §51).

import { el } from '../util/dom.js';
import type { AppState } from '../store.js';

export interface ComposerHandlers {
  submit: (text: string) => void | Promise<void>;
  interrupt: () => void | Promise<void>;
  newSession: () => void | Promise<void>;
}

const MAX_ROWS = 12;

export class Composer {
  readonly root: HTMLElement;
  readonly input: HTMLTextAreaElement;
  private readonly send: HTMLButtonElement;
  private readonly stop: HTMLButtonElement;
  private readonly hint: HTMLElement;
  private sending = false;

  constructor(private readonly handlers: ComposerHandlers) {
    this.input = el('textarea', {
      class: 'composer-input',
      id: 'composer-input',
      attrs: { rows: '1', placeholder: 'Ask Boop… (Enter to send, Shift+Enter for a new line)', spellcheck: 'false' },
      aria: { label: 'Prompt' },
    }) as HTMLTextAreaElement;

    this.input.addEventListener('input', () => this.autosize());
    this.input.addEventListener('keydown', (ev: KeyboardEvent) => {
      if (ev.key === 'Enter' && !ev.shiftKey && !ev.altKey) {
        ev.preventDefault();
        void this.submit();
      }
    });

    this.send = el('button', {
      class: 'btn btn-primary',
      type: 'button',
      text: 'Send',
      on: { click: () => void this.submit() },
    }) as HTMLButtonElement;

    this.stop = el('button', {
      class: 'btn btn-stop',
      type: 'button',
      text: 'Stop',
      title: 'Cancel the running model call, command or agent',
      disabled: true,
      on: { click: () => void this.handlers.interrupt() },
    }) as HTMLButtonElement;

    const fresh = el('button', {
      class: 'btn btn-secondary',
      type: 'button',
      text: 'New session',
      on: { click: () => void this.handlers.newSession() },
    });

    this.hint = el('p', { class: 'composer-hint', role: 'status', aria: { live: 'polite' }, text: '' });

    this.root = el(
      'form',
      {
        class: 'composer',
        attrs: { autocomplete: 'off' },
        on: {
          submit: (ev: Event) => {
            ev.preventDefault();
            void this.submit();
          },
        },
      },
      this.input,
      el('div', { class: 'composer-actions' }, this.stop, fresh, this.send),
      this.hint,
    );
  }

  focus(): void {
    this.input.focus();
  }

  update(state: AppState): void {
    this.stop.disabled = !state.busy;
    this.root.classList.toggle('is-busy', state.busy);
    const offline = state.connection !== 'open';
    this.send.disabled = this.sending;
    this.hint.textContent = offline
      ? 'Not connected — messages will fail until the socket is back.'
      : state.busy
        ? 'Boop is working. Stop cancels the current operation.'
        : '';
  }

  setError(message: string): void {
    this.hint.textContent = message;
  }

  private autosize(): void {
    const style = this.input.style;
    style.height = 'auto';
    const lineHeight = 22;
    const rows = Math.min(MAX_ROWS, Math.max(1, this.input.value.split('\n').length));
    style.height = `${Math.max(this.input.scrollHeight || rows * lineHeight, rows * lineHeight)}px`;
  }

  private async submit(): Promise<void> {
    const text = this.input.value.trim();
    if (text === '' || this.sending) return;
    this.sending = true;
    this.send.disabled = true;
    try {
      await this.handlers.submit(text);
      this.input.value = '';
      this.autosize();
    } finally {
      this.sending = false;
      this.send.disabled = false;
      this.input.focus();
    }
  }
}
