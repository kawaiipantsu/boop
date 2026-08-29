// WebSocket transport for the §25 event stream.
//
// Responsibilities: connect, survive the server restarting under us, and hand
// well-formed events to one callback. It owns reconnection policy and nothing
// else — no UI, no application state.

import { websocketUrl } from './api.js';
import {
  parseServerMessage, PROTOCOL_VERSION,
  type ApprovalEvent, type BoopEvent, type HelloData,
} from './protocol.js';

export type ConnectionState = 'connecting' | 'open' | 'reconnecting' | 'offline';

export interface SocketOptions {
  /** Server path of the event stream (web.EventsPath). */
  path?: string;
  /** A core bus event, unwrapped from the `event` envelope. */
  onEvent: (event: BoopEvent) => void;
  onState: (state: ConnectionState, detail: { attempt: number; nextRetryMs: number }) => void;
  /** Connect-time snapshot; authoritative for session and approval queue. */
  onHello?: (hello: HelloData) => void;
  /** A change to the approval queue, from any connected frontend (§50). */
  onApproval?: (event: ApprovalEvent) => void;
  /** The server rejected a frame we sent, or reported a runtime problem. */
  onServerError?: (message: string, id: string) => void;
  /** We fell behind and the server dropped frames; the caller must resync. */
  onDropped?: (count: number) => void;
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

/** web.EventsPath. */
export const DEFAULT_PATH = '/api/events';

export class EventSocket {
  private readonly opts: SocketOptions & {
    path: string;
    factory: (url: string) => WebSocketLike;
    setTimer: (fn: () => void, ms: number) => number;
    clearTimer: (handle: number) => void;
    random: () => number;
  };

  private socket: WebSocketLike | null = null;
  private attempt = 0;
  private timer: number | null = null;
  private stopped = false;
  private nextFrameID = 0;

  constructor(options: SocketOptions) {
    this.opts = {
      ...options,
      path: options.path ?? DEFAULT_PATH,
      factory: options.factory ?? ((url) => new WebSocket(url) as unknown as WebSocketLike),
      setTimer: options.setTimer ?? ((fn, ms) => setTimeout(fn, ms) as unknown as number),
      clearTimer: options.clearTimer ?? ((h) => clearTimeout(h)),
      random: options.random ?? Math.random,
    };
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

  /**
   * Best-effort client→server frame in the server's envelope
   * (web.ClientMessageEnvelope). Returns false when the socket is down, which
   * is the caller's cue to fall back to REST.
   */
  send(type: string, data?: unknown): boolean {
    if (!this.socket || this.socket.readyState !== OPEN) return false;
    this.nextFrameID += 1;
    const frame: Record<string, unknown> = { type, id: `c${this.nextFrameID}` };
    if (data !== undefined) frame['data'] = data;
    try {
      this.socket.send(JSON.stringify(frame));
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

  private open(): void {
    if (this.stopped) return;
    const path = this.opts.path;
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
      this.opts.onState('open', { attempt: 0, nextRetryMs: 0 });
      this.opts.onOpen?.();
    };
    socket.onmessage = (ev) => {
      if (this.socket !== socket) return;
      this.receive(ev.data);
    };
    socket.onerror = () => {
      /* onclose always follows; handled there. */
    };
    socket.onclose = () => {
      if (this.socket !== socket) return;
      this.socket = null;
      this.scheduleRetry();
    };
  }

  /** Decodes one server frame and routes it. Exposed for tests. */
  receive(raw: unknown): void {
    const msg = parseServerMessage(raw);
    if (!msg) return;
    switch (msg.kind) {
      case 'hello':
        if (msg.hello.protocol !== PROTOCOL_VERSION) {
          this.opts.onServerError?.(
            `This page speaks event protocol ${PROTOCOL_VERSION} but the server speaks ${msg.hello.protocol}. Reload after rebuilding the WebUI.`,
            '',
          );
        }
        this.opts.onHello?.(msg.hello);
        break;
      case 'event':
        this.opts.onEvent(msg.event);
        break;
      case 'approval':
        this.opts.onApproval?.(msg.approval);
        break;
      case 'error':
        this.opts.onServerError?.(msg.message, msg.id);
        break;
      case 'dropped':
        this.opts.onDropped?.(msg.count);
        break;
      case 'ack':
      case 'pong':
        break;
    }
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
