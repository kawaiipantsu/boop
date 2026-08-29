import assert from 'node:assert/strict';
import { afterEach, beforeEach, describe, it } from 'node:test';

import { setupDom } from '../testing/dom.js';
import { append, clear, el, isNearBottom, replaceChildren, trapFocus } from './dom.js';

let dom: ReturnType<typeof setupDom>;
beforeEach(() => {
  dom = setupDom();
});
afterEach(() => dom.cleanup());

describe('el', () => {
  it('sets text as a text node, so markup in a value cannot become markup', () => {
    const node = el('p', { text: '<b>bold</b>' });
    assert.equal(node.querySelectorAll('b').length, 0);
    assert.equal(node.textContent, '<b>bold</b>');
  });

  it('treats string children as text too', () => {
    const node = el('div', {}, '<script>x</script>');
    assert.equal(node.querySelectorAll('script').length, 0);
    assert.equal(node.textContent, '<script>x</script>');
  });

  it('applies aria, data and arbitrary attributes', () => {
    const node = el('div', {
      class: 'x',
      aria: { live: 'polite', hidden: true },
      data: { tool: 'run' },
      attrs: { tabindex: '0' },
    });
    assert.equal(node.getAttribute('aria-live'), 'polite');
    assert.equal(node.getAttribute('aria-hidden'), 'true');
    assert.equal(node.getAttribute('data-tool'), 'run');
    assert.equal(node.getAttribute('tabindex'), '0');
    assert.equal(node.className, 'x');
  });

  it('skips null and false children', () => {
    const node = el('div', {}, 'a', null, false, undefined, 'b');
    assert.equal(node.childNodes.length, 2);
  });

  it('wires listeners', () => {
    let clicked = 0;
    const node = el('button', { on: { click: () => (clicked += 1) } });
    node.click();
    assert.equal(clicked, 1);
  });
});

describe('children helpers', () => {
  it('clears and replaces', () => {
    const node = el('div', {}, 'a', 'b');
    clear(node);
    assert.equal(node.childNodes.length, 0);
    append(node, ['c']);
    assert.equal(node.textContent, 'c');
    replaceChildren(node, ['d']);
    assert.equal(node.textContent, 'd');
  });
});

describe('trapFocus', () => {
  it('wraps forwards and backwards, and releases cleanly', () => {
    const first = el('button', { text: 'first' });
    const last = el('button', { text: 'last' });
    const box = el('div', {}, first, last);
    document.body.appendChild(box);
    const release = trapFocus(box);

    last.focus();
    box.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Tab', bubbles: true }));
    assert.equal(document.activeElement, first);

    box.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, bubbles: true }));
    assert.equal(document.activeElement, last);

    release();
    first.focus();
    box.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Tab', bubbles: true }));
    assert.equal(document.activeElement, first);
  });
});

describe('isNearBottom', () => {
  it('uses the scroll geometry', () => {
    const node = { scrollHeight: 1000, scrollTop: 900, clientHeight: 100 } as HTMLElement;
    assert.equal(isNearBottom(node), true);
    const scrolledUp = { scrollHeight: 1000, scrollTop: 100, clientHeight: 100 } as HTMLElement;
    assert.equal(isNearBottom(scrolledUp), false);
  });
});
