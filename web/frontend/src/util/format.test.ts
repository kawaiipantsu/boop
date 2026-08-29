import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { formatCompact, formatCost, formatDuration, formatNumber, truncate } from './format.js';

describe('format', () => {
  it('formats durations across the useful range', () => {
    assert.equal(formatDuration(null), '');
    assert.equal(formatDuration(0.4), '<1ms');
    assert.equal(formatDuration(250), '250ms');
    assert.equal(formatDuration(1500), '1.50s');
    assert.equal(formatDuration(45_000), '45.0s');
    assert.equal(formatDuration(90_000), '1m 30s');
    assert.equal(formatDuration(3_930_000), '1h 05m');
  });

  it('formats token counts compactly', () => {
    assert.equal(formatCompact(999), '999');
    assert.equal(formatCompact(1500), '1.5k');
    assert.equal(formatCompact(120_000), '120k');
    assert.equal(formatCompact(34_000_000), '34M');
    assert.equal(formatCompact(3_400_000), '3.4M');
    assert.equal(formatNumber(1234567), '1,234,567');
  });

  it('formats cost with sensible precision', () => {
    assert.equal(formatCost(0, 'USD'), '0 USD');
    assert.equal(formatCost(0.0123, 'USD'), '0.0123 USD');
    assert.equal(formatCost(12.5, 'EUR'), '12.50 EUR');
  });

  it('truncates on a single line', () => {
    assert.equal(truncate('a\n b   c'), 'a b c');
    assert.equal(truncate('abcdef', 4), 'abc…');
  });
});
