// REST client for the Boop Web API (PROJECT.md §24).
//
// One module, no framework. Every request is same-origin and relative to the
// document base, so the UI works when the Go server mounts it at "/" and also
// when an upstream reverse proxy adds a path prefix (§22).

import {
  parseAgentList, parseModels, parseProviders, parseSessions, parseStatus,
  type AgentView, type GrantScope, type ModelView, type ProviderView,
  type SessionView, type StatusView,
} from './protocol.js';

export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export interface SendResult {
  sessionId: string;
}

function baseUrl(): string {
  const base = typeof document !== 'undefined' && document.baseURI ? document.baseURI : '/';
  return base.endsWith('/') ? base : `${base.slice(0, base.lastIndexOf('/'))}/`;
}

export function resolveUrl(path: string): string {
  const rel = path.replace(/^\//, '');
  try {
    return new URL(rel, baseUrl()).toString();
  } catch {
    return `/${rel}`;
  }
}

/** WebSocket URL for a server path, honouring http→ws / https→wss. */
export function websocketUrl(path: string): string {
  const url = new URL(resolveUrl(path));
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.toString();
}

export interface RequestOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
}

async function request(path: string, opts: RequestOptions = {}): Promise<unknown> {
  const init: RequestInit = {
    method: opts.method ?? 'GET',
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
  };
  if (opts.signal) init.signal = opts.signal;
  if (opts.body !== undefined) {
    init.headers = { ...(init.headers as Record<string, string>), 'Content-Type': 'application/json' };
    init.body = JSON.stringify(opts.body);
  }

  let res: Response;
  try {
    res = await fetch(resolveUrl(path), init);
  } catch (err) {
    throw new ApiError(0, err instanceof Error ? err.message : 'network request failed');
  }

  const text = await res.text();
  let data: unknown = null;
  if (text !== '') {
    try {
      data = JSON.parse(text) as unknown;
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    const detail =
      typeof data === 'object' && data !== null && 'error' in data
        ? String((data as Record<string, unknown>)['error'])
        : typeof data === 'string' && data !== ''
          ? data
          : res.statusText || `HTTP ${res.status}`;
    throw new ApiError(res.status, detail);
  }
  return data;
}

export const api = {
  raw: request,

  async status(signal?: AbortSignal): Promise<StatusView> {
    return parseStatus(await request('/api/status', { signal }));
  },
  async agents(signal?: AbortSignal): Promise<AgentView[]> {
    return parseAgentList(await request('/api/agents', { signal }));
  },
  async models(signal?: AbortSignal): Promise<ModelView[]> {
    return parseModels(await request('/api/models', { signal }));
  },
  async providers(signal?: AbortSignal): Promise<ProviderView[]> {
    return parseProviders(await request('/api/providers', { signal }));
  },
  async sessions(signal?: AbortSignal): Promise<SessionView[]> {
    return parseSessions(await request('/api/sessions', { signal }));
  },
  async stats(signal?: AbortSignal): Promise<unknown> {
    return request('/api/stats', { signal });
  },
  async config(signal?: AbortSignal): Promise<unknown> {
    return request('/api/config', { signal });
  },
  async saveConfig(config: unknown): Promise<unknown> {
    return request('/api/config', { method: 'PUT', body: config });
  },

  /** POST /api/message — submit a user turn. */
  async sendMessage(text: string, sessionId?: string): Promise<SendResult> {
    const body: Record<string, unknown> = { text, message: text };
    if (sessionId) body['session_id'] = sessionId;
    const data = await request('/api/message', { method: 'POST', body });
    const rec = typeof data === 'object' && data !== null ? (data as Record<string, unknown>) : {};
    return { sessionId: typeof rec['session_id'] === 'string' ? rec['session_id'] : (sessionId ?? '') };
  },

  /** POST /api/approval — resolve one pending approval (§50). */
  async resolveApproval(id: string, approved: boolean, scope: GrantScope = 'once'): Promise<void> {
    await request('/api/approval', { method: 'POST', body: { id, approved, scope } });
  },

  /** POST /api/session — start a fresh session. */
  async newSession(): Promise<string> {
    const data = await request('/api/session', { method: 'POST', body: {} });
    const rec = typeof data === 'object' && data !== null ? (data as Record<string, unknown>) : {};
    return typeof rec['session_id'] === 'string'
      ? rec['session_id']
      : typeof rec['id'] === 'string'
        ? rec['id']
        : '';
  },

  /**
   * Cancel the active operation (§51). The WebSocket carries this when it is
   * open; this HTTP path is the fallback for a dropped socket. A 404 means the
   * server has not implemented it, which must not surface as a UI error.
   */
  async interrupt(sessionId?: string): Promise<void> {
    const body: Record<string, unknown> = {};
    if (sessionId) body['session_id'] = sessionId;
    try {
      await request('/api/interrupt', { method: 'POST', body });
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) return;
      throw err;
    }
  },
};

export type Api = typeof api;
