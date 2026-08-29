// Minimal ambient declaration so we do not need @types/jsdom for the two
// members the test harness touches.
declare module 'jsdom' {
  export class JSDOM {
    constructor(html?: string, options?: Record<string, unknown>);
    readonly window: Window & typeof globalThis & { close(): void };
  }
}
