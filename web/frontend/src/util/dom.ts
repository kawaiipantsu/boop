// Tiny DOM helpers.
//
// There is deliberately no HTML-string API here. Everything this UI renders
// may have come from a model or from a command's stdout, and the only safe
// default for untrusted text is a text node. `el()` cannot be handed markup:
// string children always become text.

export type Child = string | number | Node | null | undefined | false;

export interface ElProps {
  class?: string;
  text?: string;
  title?: string;
  id?: string;
  type?: string;
  role?: string;
  href?: string;
  value?: string;
  disabled?: boolean;
  hidden?: boolean;
  tabIndex?: number;
  /** data-* attributes, without the prefix. */
  data?: Record<string, string>;
  /** aria-* attributes, without the prefix. */
  aria?: Record<string, string | boolean | number>;
  /** Any other attribute, set verbatim. */
  attrs?: Record<string, string>;
  on?: Partial<Record<keyof HTMLElementEventMap | string, (ev: Event) => void>>;
}

export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props: ElProps = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (props.class) node.className = props.class;
  if (props.id) node.id = props.id;
  if (props.title) node.title = props.title;
  if (props.role) node.setAttribute('role', props.role);
  if (props.type) node.setAttribute('type', props.type);
  if (props.href) node.setAttribute('href', props.href);
  if (props.value !== undefined) (node as unknown as { value: string }).value = props.value;
  if (props.disabled) node.setAttribute('disabled', '');
  if (props.hidden) node.hidden = true;
  if (props.tabIndex !== undefined) node.tabIndex = props.tabIndex;
  if (props.text !== undefined) node.textContent = props.text;
  if (props.data) {
    for (const [k, v] of Object.entries(props.data)) node.setAttribute(`data-${k}`, v);
  }
  if (props.aria) {
    for (const [k, v] of Object.entries(props.aria)) node.setAttribute(`aria-${k}`, String(v));
  }
  if (props.attrs) {
    for (const [k, v] of Object.entries(props.attrs)) node.setAttribute(k, v);
  }
  if (props.on) {
    for (const [k, fn] of Object.entries(props.on)) {
      if (fn) node.addEventListener(k, fn as EventListener);
    }
  }
  append(node, children);
  return node;
}

export function append(parent: Node, children: Child[]): void {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    parent.appendChild(
      typeof child === 'string' || typeof child === 'number'
        ? document.createTextNode(String(child))
        : child,
    );
  }
}

export function clear(node: Node): void {
  while (node.firstChild) node.removeChild(node.firstChild);
}

export function replaceChildren(node: Node, children: Child[]): void {
  clear(node);
  append(node, children);
}

/** A screen-reader-only text node holder. */
export function srOnly(text: string): HTMLElement {
  return el('span', { class: 'sr-only', text });
}

/**
 * Traps Tab inside `container` and returns a release function. Used by the
 * approval dialog: an approval must not be dismissible by tabbing past it.
 */
export function trapFocus(container: HTMLElement): () => void {
  const selector = [
    'a[href]', 'button:not([disabled])', 'textarea:not([disabled])',
    'input:not([disabled])', 'select:not([disabled])', '[tabindex]:not([tabindex="-1"])',
  ].join(',');

  const onKeyDown = (ev: KeyboardEvent): void => {
    if (ev.key !== 'Tab') return;
    const items = Array.from(container.querySelectorAll<HTMLElement>(selector)).filter(
      (n) => !n.hasAttribute('disabled') && n.closest('[hidden]') === null,
    );
    if (items.length === 0) {
      ev.preventDefault();
      return;
    }
    const first = items[0] as HTMLElement;
    const last = items[items.length - 1] as HTMLElement;
    const active = document.activeElement;
    if (ev.shiftKey && (active === first || !container.contains(active))) {
      ev.preventDefault();
      last.focus();
    } else if (!ev.shiftKey && active === last) {
      ev.preventDefault();
      first.focus();
    }
  };

  container.addEventListener('keydown', onKeyDown as EventListener);
  return () => container.removeEventListener('keydown', onKeyDown as EventListener);
}

/** True when the element is scrolled to within `slack` px of its bottom. */
export function isNearBottom(node: HTMLElement, slack = 64): boolean {
  return node.scrollHeight - node.scrollTop - node.clientHeight <= slack;
}
