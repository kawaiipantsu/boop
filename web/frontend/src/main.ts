// Entry point.

import { mount } from './app.js';

const root = document.getElementById('root');
if (root) {
  mount(root);
} else {
  // Should be impossible: index.html ships the container.
  document.body.textContent = 'Boop could not find its mount point.';
}
