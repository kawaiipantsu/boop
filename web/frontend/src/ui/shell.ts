// Navigation shell: an ARIA tab list over the panels from §26.
//
// The approval region deliberately lives outside the tab panels. An approval
// is not a tab you can navigate away from.

import { el } from '../util/dom.js';

export interface View {
  id: string;
  label: string;
  node: HTMLElement;
  /** Called the first time the view is shown, and on every later activation. */
  onShow?: () => void;
}

export class Shell {
  readonly root: HTMLElement;
  private readonly tabs = new Map<string, HTMLButtonElement>();
  private readonly views = new Map<string, View>();
  private readonly tablist: HTMLElement;
  private readonly main: HTMLElement;
  private active = '';

  constructor(header: HTMLElement, approvals: HTMLElement, views: View[]) {
    this.tablist = el('div', { class: 'tabs', role: 'tablist', aria: { label: 'Sections' } });
    this.main = el('main', { class: 'main' });

    for (const view of views) {
      this.views.set(view.id, view);
      const tab = el('button', {
        class: 'tab',
        type: 'button',
        role: 'tab',
        id: `tab-${view.id}`,
        text: view.label,
        tabIndex: -1,
        aria: { selected: 'false', controls: `panel-${view.id}` },
        on: { click: () => this.select(view.id) },
      }) as HTMLButtonElement;
      this.tabs.set(view.id, tab);
      this.tablist.appendChild(tab);

      view.node.id = `panel-${view.id}`;
      view.node.setAttribute('role', 'tabpanel');
      view.node.setAttribute('aria-labelledby', `tab-${view.id}`);
      view.node.hidden = true;
      this.main.appendChild(view.node);
    }

    this.tablist.addEventListener('keydown', (ev: Event) => this.onKeyDown(ev as KeyboardEvent));

    this.root = el('div', { class: 'shell' }, header, this.tablist, approvals, this.main);

    const first = views[0];
    if (first) this.select(first.id);
  }

  select(id: string): void {
    const view = this.views.get(id);
    if (!view) return;
    this.active = id;
    for (const [key, tab] of this.tabs) {
      const selected = key === id;
      tab.setAttribute('aria-selected', String(selected));
      tab.classList.toggle('is-active', selected);
      tab.tabIndex = selected ? 0 : -1;
      const node = this.views.get(key)?.node;
      if (node) node.hidden = !selected;
    }
    view.onShow?.();
  }

  get current(): string {
    return this.active;
  }

  private onKeyDown(ev: KeyboardEvent): void {
    const ids = Array.from(this.tabs.keys());
    const index = ids.indexOf(this.active);
    if (index === -1) return;
    let next = -1;
    if (ev.key === 'ArrowRight') next = (index + 1) % ids.length;
    else if (ev.key === 'ArrowLeft') next = (index - 1 + ids.length) % ids.length;
    else if (ev.key === 'Home') next = 0;
    else if (ev.key === 'End') next = ids.length - 1;
    if (next === -1) return;
    ev.preventDefault();
    const id = ids[next] as string;
    this.select(id);
    this.tabs.get(id)?.focus();
  }
}
