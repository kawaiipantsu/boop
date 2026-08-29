// REST client for the Boop Web API (PROJECT.md §24).
//
// One module, no framework. Every request is same-origin and relative to the
// document base, so the UI works when the Go server mounts it at "/" and also
// when an upstream reverse proxy adds a path prefix (§22).

import { accessToken, TOKEN_QUERY_PARAM } from './auth.js';
import {
  isRecord, parseAgentList, parseModels, parsePendingApprovals, parseProviders,
  parseSessions, parseStatus, str,
  type AgentView, type Approval, type GrantScope, type ModelView,
  type ProviderView, type SessionView, type StatusView,
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

/**
 * WebSocket URL for a server path, honouring http→ws / https→wss.
 *
 * A browser cannot set an Authorization header on an upgrade, so when a token
 * is configured it goes in the query string, which is what web/auth.go accepts
 * as the fallback. Same-origin only, so it never leaves this machine.
 */
export function websocketUrl(path: string): string {
  const url = new URL(resolveUrl(path));
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  const token = accessToken();
  if (token !== '') url.searchParams.set(TOKEN_QUERY_PARAM, token);
  return url.toString();
}

export interface RequestOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
}

async function request(path: string, opts: RequestOptions = {}): Promise<unknown> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  const token = accessToken();
  if (token !== '') headers['Authorization'] = `Bearer ${token}`;
  const init: RequestInit = {
    method: opts.method ?? 'GET',
    credentials: 'same-origin',
    headers,
  };
  if (opts.signal) init.signal = opts.signal;
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json';
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
    throw new ApiError(res.status, errorMessageOf(data) || res.statusText || `HTTP ${res.status}`);
  }
  return data;
}

/** Unwraps the `{error:{code,message,details}}` envelope from web/api.go. */
function errorMessageOf(data: unknown): string {
  if (typeof data === 'string') return data;
  if (!isRecord(data)) return '';
  const body = isRecord(data['error']) ? data['error'] : data;
  const message = str(body['message']);
  const details = Array.isArray(body['details']) ? body['details'].map((d) => str(d)).filter(Boolean) : [];
  return details.length > 0 ? `${message} (${details.join('; ')})` : message;
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
  /** GET /api/config returns `{config, secrets, path, warnings}`. */
  async config(signal?: AbortSignal): Promise<unknown> {
    return request('/api/config', { signal });
  },
  /** PUT /api/config takes `{config}`; the server rejects unknown fields. */
  async saveConfig(config: unknown): Promise<unknown> {
    return request('/api/config', { method: 'PUT', body: { config } });
  },

  /**
   * POST /api/message — submit a user turn.
   *
   * `async: true` because the answer belongs on the event stream; a
   * synchronous turn would hold an HTTP connection open for the whole run.
   */
  async sendMessage(content: string, sessionId?: string): Promise<SendResult> {
    const body: Record<string, unknown> = { content, async: true };
    if (sessionId) body['session_id'] = sessionId;
    const data = await request('/api/message', { method: 'POST', body });
    const rec = isRecord(data) ? data : {};
    return { sessionId: str(rec['session_id'], sessionId ?? '') };
  },

  /** GET /api/approval — the pending queue, used to resync after a reconnect. */
  async approvals(signal?: AbortSignal): Promise<Approval[]> {
    return parsePendingApprovals(await request('/api/approval', { signal }));
  },

  /** GET /api/tools — the registered tools and the current execution mode. */
  async tools(signal?: AbortSignal): Promise<unknown> {
    return request('/api/tools', { signal });
  },

  /** POST /api/approval — resolve one pending approval (§50). */
  async resolveApproval(id: string, approved: boolean, scope: GrantScope = 'once'): Promise<void> {
    await request('/api/approval', { method: 'POST', body: { id, approved, scope } });
  },

  /** POST /api/session — start a fresh session, or resume an existing one. */
  async newSession(resume?: string): Promise<string> {
    const body: Record<string, unknown> = {};
    if (resume) body['resume'] = resume;
    const data = await request('/api/session', { method: 'POST', body });
    const rec = isRecord(data) ? data : {};
    const session = isRecord(rec['session']) ? rec['session'] : rec;
    return str(session['id'], str(rec['session_id']));
  },

  /**
   * Cancel the active operation (§51).
   *
   * The WebSocket `cancel` frame is the real mechanism (web/websocket.go); this
   * is only reachable with the socket down, and the server has no REST route
   * for it today. A 404 therefore means "nothing to do", not "broken".
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
