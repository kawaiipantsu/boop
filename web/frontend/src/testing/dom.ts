// jsdom bootstrap for the DOM-facing unit tests.
//
// Only used by *.test.ts; nothing here is bundled into dist.

import { JSDOM } from 'jsdom';

export interface DomHandle {
  window: Window & typeof globalThis;
  cleanup: () => void;
}

const GLOBALS = [
  'window', 'document', 'navigator', 'HTMLElement', 'HTMLTextAreaElement',
  'HTMLButtonElement', 'Node', 'Text', 'Event', 'KeyboardEvent', 'CustomEvent',
  'getComputedStyle', 'requestAnimationFrame', 'cancelAnimationFrame',
] as const;

export function setupDom(html = '<!doctype html><html><body><div id="root"></div></body></html>'): DomHandle {
  const dom = new JSDOM(html, { url: 'http://127.0.0.1:8585/', pretendToBeVisual: true });
  const win = dom.window as unknown as Window & typeof globalThis;
  const g = globalThis as unknown as Record<string, unknown>;
  const saved: Record<string, unknown> = {};
  for (const key of GLOBALS) {
    saved[key] = g[key];
    g[key] = (win as unknown as Record<string, unknown>)[key];
  }
  // jsdom's rAF is tied to a real timer; tests drive Transcript.flush() by
  // hand instead, so make scheduling a no-op that never fires late.
  g['requestAnimationFrame'] = (): number => 0;
  g['cancelAnimationFrame'] = (): void => undefined;

  return {
    window: win,
    cleanup: () => {
      for (const key of GLOBALS) g[key] = saved[key];
      dom.window.close();
    },
  };
}

/** Convenience: the text content of the first match, or ''. */
export function textOf(root: ParentNode, selector: string): string {
  return root.querySelector(selector)?.textContent ?? '';
}

export function all(root: ParentNode, selector: string): Element[] {
  return Array.from(root.querySelectorAll(selector));
}
