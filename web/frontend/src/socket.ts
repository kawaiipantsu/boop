// WebSocket transport for the §25 event stream.
//
// Responsibilities: connect, survive the server restarting under us, and hand
// well-formed events to one callback. It owns reconnection policy and nothing
// else — no UI, no application state.

import { websocketUrl } from './api.js';
import { parseEvent, type BoopEvent } from './protocol.js';

export type ConnectionState = 'connecting' | 'open' | 'reconnecting' | 'offline';

export interface SocketOptions {
  /** Candidate server paths, tried in order until one opens. */
  paths?: string[];
  onEvent: (event: BoopEvent) => void;
  onState: (state: ConnectionState, detail: { attempt: number; nextRetryMs: number }) => void;
  /** Fires on every successful (re)connect so the caller can resync via REST. */
  onOpen?: () => void;
  /** Injectable for tests. */
  factory?: (url: string) => WebSocketLike;
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => number;
  clearTimer?: (handle: number) => void;
  random?: () => number;
}

export interface WebSocketLike {
  readyState: number;
  send(data: string): void;
  close(): void;
  onopen: ((ev: unknown) => void) | null;
  onclose: ((ev: unknown) => void) | null;
  onerror: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
}

const OPEN = 1;
const BASE_DELAY_MS = 500;
const MAX_DELAY_MS = 30_000;

/** Default event-stream paths, most likely first. */
export const DEFAULT_PATHS = ['/api/events', '/api/ws', '/ws'];

export class EventSocket {
  private readonly opts: Required<Omit<SocketOptions, 'onOpen'>> & { onOpen?: () => void };
  private readonly paths: string[];
  private socket: WebSocketLike | null = null;
  private pathIndex = 0;
  /** Locked to the path that last opened successfully; stops path probing. */
  private lockedPath: string | null = null;
  private attempt = 0;
  private timer: number | null = null;
  private stopped = false;

  constructor(options: SocketOptions) {
    this.paths = options.paths && options.paths.length > 0 ? options.paths : DEFAULT_PATHS;
    this.opts = {
      paths: this.paths,
      onEvent: options.onEvent,
      onState: options.onState,
      factory: options.factory ?? ((url) => new WebSocket(url) as unknown as WebSocketLike),
      now: options.now ?? (() => Date.now()),
      setTimer: options.setTimer ?? ((fn, ms) => setTimeout(fn, ms) as unknown as number),
      clearTimer: options.clearTimer ?? ((h) => clearTimeout(h)),
      random: options.random ?? Math.random,
    };
    if (options.onOpen) this.opts.onOpen = options.onOpen;
  }

  get connected(): boolean {
    return this.socket !== null && this.socket.readyState === OPEN;
  }

  start(): void {
    this.stopped = false;
    this.open();
  }

  stop(): void {
    this.stopped = true;
    this.cancelTimer();
    const s = this.socket;
    this.socket = null;
    if (s) {
      s.onopen = s.onclose = s.onerror = null;
      s.onmessage = null;
      try {
        s.close();
      } catch {
        /* already gone */
      }
    }
  }

  /** Reconnect now, e.g. the tab became visible or the browser came online. */
  retryNow(): void {
    if (this.stopped || this.connected) return;
    this.cancelTimer();
    this.open();
  }

  /** Best-effort client→server frame. Returns false when the socket is down. */
  send(payload: unknown): boolean {
    if (!this.socket || this.socket.readyState !== OPEN) return false;
    try {
      this.socket.send(JSON.stringify(payload));
      return true;
    } catch {
      return false;
    }
  }

  /** Backoff for the given attempt: exponential, capped, with ±20% jitter. */
  delayFor(attempt: number): number {
    const raw = Math.min(BASE_DELAY_MS * 2 ** Math.max(0, attempt - 1), MAX_DELAY_MS);
    const jitter = raw * 0.2 * (this.opts.random() * 2 - 1);
    return Math.max(BASE_DELAY_MS / 2, Math.round(raw + jitter));
  }

  private nextPath(): string {
    if (this.lockedPath) return this.lockedPath;
    const path = this.paths[this.pathIndex % this.paths.length] as string;
    return path;
  }

  private open(): void {
    if (this.stopped) return;
    const path = this.nextPath();
    this.opts.onState(this.attempt === 0 ? 'connecting' : 'reconnecting', {
      attempt: this.attempt,
      nextRetryMs: 0,
    });

    let socket: WebSocketLike;
    try {
      socket = this.opts.factory(websocketUrl(path));
    } catch {
      this.scheduleRetry();
      return;
    }
    this.socket = socket;

    socket.onopen = () => {
      if (this.socket !== socket) return;
      this.attempt = 0;
      this.lockedPath = path;
      this.opts.onState('open', { attempt: 0, nextRetryMs: 0 });
      this.opts.onOpen?.();
    };
    socket.onmessage = (ev) => {
      if (this.socket !== socket) return;
      const parsed = parseEvent(ev.data);
      if (parsed) this.opts.onEvent(parsed);
    };
    socket.onerror = () => {
      /* onclose always follows; handled there. */
    };
    socket.onclose = () => {
      if (this.socket !== socket) return;
      this.socket = null;
      if (!this.lockedPath) this.pathIndex += 1;
      this.scheduleRetry();
    };
  }

  private scheduleRetry(): void {
    if (this.stopped) return;
    this.attempt += 1;
    const delay = this.delayFor(this.attempt);
    this.opts.onState(this.attempt >= 2 ? 'offline' : 'reconnecting', {
      attempt: this.attempt,
      nextRetryMs: delay,
    });
    this.timer = this.opts.setTimer(() => {
      this.timer = null;
      this.open();
    }, delay);
  }

  private cancelTimer(): void {
    if (this.timer !== null) {
      this.opts.clearTimer(this.timer);
      this.timer = null;
    }
  }
}
