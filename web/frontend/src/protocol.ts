// Wire contract between the Boop Go server and this frontend.
//
// Everything the server sends is parsed here and nowhere else, so a shape
// change on the Go side is a one-file fix (PROJECT.md §24, §25).
//
// The server is permitted to send fields we do not know about; parsing is
// deliberately tolerant. It is NOT permitted to make us trust its strings:
// every value that reaches the DOM goes through textContent, never innerHTML.

/** Event names published on the core bus (internal/app/events.go). */
export const EVENT_TYPES = [
  'session.started',
  'session.completed',
  'prompt.received',
  'model.request.started',
  'model.token',
  'model.reasoning',
  'model.response.completed',
  'agent.created',
  'agent.status.changed',
  'tool.requested',
  'tool.completed',
  'approval.requested',
  'approval.received',
  'command.started',
  'command.stdout',
  'command.stderr',
  'command.completed',
  'test.started',
  'test.completed',
  'error',
] as const;

export type EventType = (typeof EVENT_TYPES)[number];

/** Mirrors Go's app.Event. */
export interface BoopEvent {
  type: EventType | string;
  session_id?: string;
  agent_id?: string;
  payload?: unknown;
  at?: string;
}

export type Risk = 'low' | 'medium' | 'high' | 'critical';
export type GrantScope = 'once' | 'session.category' | 'session.command';
export type ExecutionMode = 'confirm' | 'auto';

/** Mirrors Go's permissions.Action. */
export interface Action {
  category: string;
  risk: Risk;
  tool: string;
  summary: string;
  detail: string;
  paths: string[];
  production: boolean;
}

/** Mirrors Go's permissions.PendingApproval, flattened for the UI. */
export interface Approval {
  id: string;
  action: Action;
  /** Evaluator reason, shown next to the action (permissions.Decision.Reason). */
  reason: string;
  requestedAt: string;
  /**
   * True when the payload carried no id and we invented one. The core emits
   * both a bare-Action `approval.requested` bus event and an id-carrying
   * `approval` frame for the same request, so this is what lets the store
   * recognise the pair and keep the resolvable one.
   */
  synthetic: boolean;
}

/** Identity of the underlying action, used to pair the two representations. */
export function approvalFingerprint(a: Approval): string {
  const { category, tool, detail, summary } = a.action;
  return [category, tool, detail, summary].join('\u0000');
}

export interface ToolCallView {
  id: string;
  tool: string;
  summary: string;
  detail: string;
  status: 'running' | 'ok' | 'error';
  durationMs: number | null;
}

export interface AgentView {
  id: string;
  name: string;
  status: string;
  task: string;
  model: string;
  operation: string;
  tools: string[];
  files: string[];
  tokens: number;
  runtimeMs: number | null;
  output: string;
}

export interface StatusView {
  version: string;
  provider: string;
  model: string;
  mode: ExecutionMode | string;
  sessionId: string;
  projectPath: string;
  agents: number;
  tokens: { prompt: number; completion: number; total: number };
  /** Only present when the server chooses to replay unanswered approvals. */
  pendingApprovals: Approval[];
}

export interface ModelView {
  id: string;
  provider: string;
  displayName: string;
  contextWindow: number;
  capabilities: string[];
}

export interface ProviderView {
  name: string;
  healthy: boolean;
  detail: string;
  models: number;
}

export interface SessionView {
  id: string;
  title: string;
  provider: string;
  model: string;
  projectPath: string;
  updatedAt: string;
}

// ---------------------------------------------------------------------------
// Tolerant readers
// ---------------------------------------------------------------------------

export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

export function str(v: unknown, fallback = ''): string {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' && Number.isFinite(v)) return String(v);
  if (typeof v === 'boolean') return String(v);
  return fallback;
}

export function num(v: unknown, fallback = 0): number {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (typeof v === 'string' && v.trim() !== '') {
    const n = Number(v);
    if (Number.isFinite(n)) return n;
  }
  return fallback;
}

export function bool(v: unknown, fallback = false): boolean {
  if (typeof v === 'boolean') return v;
  if (v === 'true') return true;
  if (v === 'false') return false;
  return fallback;
}

export function strList(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  return v.map((x) => str(x)).filter((x) => x !== '');
}

function pick(rec: Record<string, unknown>, ...keys: string[]): unknown {
  for (const k of keys) {
    if (rec[k] !== undefined && rec[k] !== null) return rec[k];
  }
  return undefined;
}

const RISKS: Risk[] = ['low', 'medium', 'high', 'critical'];

export function asRisk(v: unknown): Risk {
  const s = str(v).toLowerCase();
  return (RISKS as string[]).includes(s) ? (s as Risk) : 'medium';
}

/**
 * Durations arrive as Go time.Duration nanoseconds, as a Go duration string
 * ("1.5s"), or as an explicit _ms field. Normalise all three to milliseconds.
 */
export function durationMs(v: unknown): number | null {
  if (typeof v === 'number' && Number.isFinite(v)) {
    // time.Duration marshals as an integer count of nanoseconds.
    return v / 1e6;
  }
  if (typeof v !== 'string' || v.trim() === '') return null;
  const text = v.trim();
  const re = /(-?\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)/g;
  const unit: Record<string, number> = {
    ns: 1e-6, us: 1e-3, 'µs': 1e-3, 'μs': 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000,
  };
  let total = 0;
  let matched = false;
  for (const m of text.matchAll(re)) {
    const scale = unit[m[2] as string];
    if (scale === undefined) continue;
    total += Number(m[1]) * scale;
    matched = true;
  }
  if (matched) return text.startsWith('-') && total > 0 ? -total : total;
  const plain = Number(text);
  return Number.isFinite(plain) ? plain / 1e6 : null;
}

export function parseEvent(raw: unknown): BoopEvent | null {
  if (typeof raw === 'string') {
    try {
      return parseEvent(JSON.parse(raw) as unknown);
    } catch {
      return null;
    }
  }
  if (!isRecord(raw)) return null;
  const type = str(raw['type']);
  if (type === '') return null;
  const ev: BoopEvent = { type };
  const sid = str(raw['session_id']);
  if (sid) ev.session_id = sid;
  const aid = str(raw['agent_id']);
  if (aid) ev.agent_id = aid;
  if (raw['payload'] !== undefined) ev.payload = raw['payload'];
  const at = str(raw['at']);
  if (at) ev.at = at;
  return ev;
}

export function parseAction(raw: unknown): Action {
  const r = isRecord(raw) ? raw : {};
  return {
    category: str(pick(r, 'category', 'Category')),
    risk: asRisk(pick(r, 'risk', 'Risk')),
    tool: str(pick(r, 'tool', 'Tool')),
    summary: str(pick(r, 'summary', 'Summary')),
    detail: str(pick(r, 'detail', 'Detail')),
    paths: strList(pick(r, 'paths', 'Paths')),
    production: bool(pick(r, 'production', 'Production')),
  };
}

/**
 * approval.requested may carry either a permissions.PendingApproval
 * ({id, action, decision, requested_at}) or a bare permissions.Action.
 * Both are accepted; a bare Action gets a synthetic id so the UI can key it,
 * and resolving it will send that id back unchanged.
 */
export function parseApproval(raw: unknown, fallbackId: () => string): Approval | null {
  if (!isRecord(raw)) return null;
  const nested = pick(raw, 'action', 'Action');
  const actionSource = isRecord(nested) ? nested : raw;
  const action = parseAction(actionSource);
  if (action.tool === '' && action.summary === '' && action.detail === '' && action.category === '') {
    return null;
  }
  const decision = pick(raw, 'decision', 'Decision');
  const reason = isRecord(decision) ? str(pick(decision, 'reason', 'Reason')) : str(pick(raw, 'reason'));
  const given = str(pick(raw, 'id', 'ID', 'approval_id'));
  return {
    id: given || fallbackId(),
    action,
    reason,
    requestedAt: str(pick(raw, 'requested_at', 'RequestedAt', 'at')),
    synthetic: given === '',
  };
}

export function parseAgent(raw: unknown): AgentView | null {
  if (!isRecord(raw)) return null;
  const id = str(pick(raw, 'id', 'ID', 'agent_id'));
  const name = str(pick(raw, 'name', 'Name'));
  if (id === '' && name === '') return null;
  const tokensRaw = pick(raw, 'tokens', 'token_use', 'total_tokens');
  const tokens = isRecord(tokensRaw) ? num(pick(tokensRaw, 'total_tokens', 'total')) : num(tokensRaw);
  let runtimeMs = durationMs(pick(raw, 'runtime_ms', 'runtime', 'elapsed_ms', 'duration'));
  if (runtimeMs === null) {
    // store.AgentRecord reports timestamps rather than a duration.
    runtimeMs = spanMs(str(pick(raw, 'started_at')), str(pick(raw, 'finished_at')));
  }
  return {
    id: id || name,
    name,
    status: str(pick(raw, 'status', 'Status'), 'unknown').toLowerCase(),
    task: str(pick(raw, 'task', 'Task')),
    model: str(pick(raw, 'model', 'Model')),
    operation: str(pick(raw, 'operation', 'current_operation', 'Operation')),
    tools: strList(pick(raw, 'tools', 'allowed_tools', 'Tools')),
    files: strList(pick(raw, 'files', 'modified_files', 'Files')),
    tokens,
    runtimeMs,
    output: str(pick(raw, 'output', 'recent_output', 'Output'), str(pick(raw, 'error'))),
  };
}

/** Milliseconds between two RFC3339 stamps; `to` defaults to now. */
export function spanMs(from: string, to: string, now = Date.now()): number | null {
  if (from === '') return null;
  const start = Date.parse(from);
  if (Number.isNaN(start)) return null;
  const end = to === '' ? now : Date.parse(to);
  return Number.isNaN(end) ? null : Math.max(0, end - start);
}

export function parseAgentList(raw: unknown): AgentView[] {
  const list = Array.isArray(raw) ? raw : isRecord(raw) ? (raw['agents'] as unknown) : null;
  if (!Array.isArray(list)) return [];
  return list.map(parseAgent).filter((a): a is AgentView => a !== null);
}

export function parseStatus(raw: unknown): StatusView {
  const r = isRecord(raw) ? raw : {};
  const usage = pick(r, 'tokens', 'usage');
  const u = isRecord(usage) ? usage : {};
  const agentsRaw = pick(r, 'agents', 'agent_count');
  const agents = Array.isArray(agentsRaw) ? agentsRaw.length : num(agentsRaw);
  const project = pick(r, 'project');
  const projectPath = isRecord(project)
    ? str(pick(project, 'path', 'root'))
    : str(pick(r, 'project_path', 'project'));
  const pending = pick(r, 'pending_approvals', 'approvals');
  let counter = 0;
  const pendingApprovals = Array.isArray(pending)
    ? pending
        .map((p) => parseApproval(p, () => `status-${(counter += 1)}`))
        .filter((a): a is Approval => a !== null)
    : [];
  return {
    version: str(pick(r, 'version')),
    provider: str(pick(r, 'provider')),
    model: str(pick(r, 'model')),
    mode: str(pick(r, 'mode', 'execution_mode')),
    sessionId: str(pick(r, 'current_session', 'session_id', 'session')),
    projectPath,
    agents,
    tokens: {
      prompt: num(pick(u, 'prompt_tokens', 'prompt', 'input')),
      completion: num(pick(u, 'completion_tokens', 'completion', 'output')),
      total: num(pick(u, 'total_tokens', 'total')),
    },
    pendingApprovals,
  };
}

export function parseModels(raw: unknown): ModelView[] {
  const list = Array.isArray(raw) ? raw : isRecord(raw) ? (raw['models'] as unknown) : null;
  if (!Array.isArray(list)) return [];
  return list.flatMap((m) => {
    if (!isRecord(m)) return [];
    const id = str(pick(m, 'id', 'ID', 'name'));
    if (id === '') return [];
    return [{
      id,
      provider: str(pick(m, 'provider', 'Provider')),
      displayName: str(pick(m, 'display_name', 'DisplayName'), id),
      contextWindow: num(pick(m, 'context_window', 'ContextWindow')),
      capabilities: strList(pick(m, 'capabilities', 'Capabilities')),
    }];
  });
}

export function parseProviders(raw: unknown): ProviderView[] {
  const list = Array.isArray(raw) ? raw : isRecord(raw) ? (raw['providers'] as unknown) : null;
  if (!Array.isArray(list)) return [];
  return list.flatMap((p) => {
    if (!isRecord(p)) return [];
    const name = str(pick(p, 'name', 'Name', 'provider'));
    if (name === '') return [];
    const health = pick(p, 'health', 'Health');
    const healthy = isRecord(health) ? bool(pick(health, 'ok', 'healthy')) : bool(pick(p, 'healthy', 'ok'));
    const detail = isRecord(health)
      ? str(pick(health, 'detail', 'message', 'error'))
      : str(pick(p, 'detail', 'message', 'error'));
    return [{ name, healthy, detail, models: num(pick(p, 'models', 'model_count')) }];
  });
}

export function parseSessions(raw: unknown): SessionView[] {
  const list = Array.isArray(raw) ? raw : isRecord(raw) ? (raw['sessions'] as unknown) : null;
  if (!Array.isArray(list)) return [];
  return list.flatMap((s) => {
    if (!isRecord(s)) return [];
    const id = str(pick(s, 'id', 'ID'));
    if (id === '') return [];
    return [{
      id,
      title: str(pick(s, 'title', 'Title'), id),
      provider: str(pick(s, 'provider')),
      model: str(pick(s, 'model')),
      projectPath: str(pick(s, 'project_path')),
      updatedAt: str(pick(s, 'updated_at', 'created_at')),
    }];
  });
}

/** Extracts streamed text from model.token / model.reasoning payloads. */
export function tokenText(payload: unknown): string {
  if (typeof payload === 'string') return payload;
  if (isRecord(payload)) {
    const v = pick(payload, 'text', 'token', 'delta', 'content');
    if (typeof v === 'string') return v;
  }
  return '';
}

/** Extracts a chunk of command output from command.stdout / command.stderr. */
export function chunkText(payload: unknown): string {
  if (typeof payload === 'string') return payload;
  if (isRecord(payload)) {
    const v = pick(payload, 'chunk', 'line', 'text', 'data', 'output');
    if (typeof v === 'string') return v;
  }
  return '';
}

/** Extracts a human message from an `error` event payload. */
export function errorText(payload: unknown): string {
  if (typeof payload === 'string') return payload;
  if (isRecord(payload)) {
    const v = pick(payload, 'message', 'error', 'err', 'detail', 'reason');
    if (typeof v === 'string' && v !== '') return v;
  }
  return 'An unspecified error occurred.';
}

// ---------------------------------------------------------------------------
// WebSocket envelope (web/websocket.go)
// ---------------------------------------------------------------------------

/** Envelope version this client understands (web.ProtocolVersion). */
export const PROTOCOL_VERSION = 1;

export interface HelloData {
  protocol: number;
  sessionId: string;
  mode: string;
  pendingApprovals: Approval[];
}

export type ApprovalEventKind = 'added' | 'resolved' | 'cancelled';

export interface ApprovalEvent {
  kind: ApprovalEventKind;
  approval: Approval | null;
  approved: boolean;
  scope: string;
}

export type ServerMessage =
  | { kind: 'hello'; hello: HelloData }
  | { kind: 'event'; event: BoopEvent }
  | { kind: 'approval'; approval: ApprovalEvent }
  | { kind: 'ack'; id: string; data: unknown }
  | { kind: 'error'; id: string; code: string; message: string }
  | { kind: 'pong'; id: string }
  | { kind: 'dropped'; count: number };

let syntheticApprovalID = 0;
function nextSyntheticID(): string {
  syntheticApprovalID += 1;
  return `pending-${syntheticApprovalID}`;
}

export function parseHello(raw: unknown): HelloData {
  const r = isRecord(raw) ? raw : {};
  const pending = r['pending_approvals'];
  return {
    protocol: num(r['protocol'], PROTOCOL_VERSION),
    sessionId: str(r['session_id']),
    mode: str(r['mode']),
    pendingApprovals: Array.isArray(pending)
      ? pending.map((p) => parseApproval(p, nextSyntheticID)).filter((a): a is Approval => a !== null)
      : [],
  };
}

export function parseApprovalEvent(raw: unknown): ApprovalEvent | null {
  if (!isRecord(raw)) return null;
  const kind = str(raw['kind']);
  if (kind !== 'added' && kind !== 'resolved' && kind !== 'cancelled') return null;
  return {
    kind,
    approval: parseApproval(raw['approval'], nextSyntheticID),
    approved: bool(raw['approved']),
    scope: str(raw['scope']),
  };
}

/**
 * Decodes one frame of the server's WebSocket envelope. Frames we do not
 * recognise return null rather than throwing: a newer server adding a message
 * type must not break an older page.
 */
export function parseServerMessage(raw: unknown): ServerMessage | null {
  if (typeof raw === 'string') {
    try {
      return parseServerMessage(JSON.parse(raw) as unknown);
    } catch {
      return null;
    }
  }
  if (!isRecord(raw)) return null;
  const id = str(raw['id']);
  switch (str(raw['type'])) {
    case 'hello':
      return { kind: 'hello', hello: parseHello(raw['data']) };
    case 'event': {
      const event = parseEvent(raw['event']);
      return event ? { kind: 'event', event } : null;
    }
    case 'approval': {
      const approval = parseApprovalEvent(raw['data']);
      return approval ? { kind: 'approval', approval } : null;
    }
    case 'ack':
      return { kind: 'ack', id, data: raw['data'] };
    case 'error': {
      const err = isRecord(raw['error']) ? raw['error'] : {};
      return { kind: 'error', id, code: str(err['code']), message: str(err['message'], 'the server rejected the request') };
    }
    case 'pong':
      return { kind: 'pong', id };
    case 'dropped': {
      const data = isRecord(raw['data']) ? raw['data'] : {};
      return { kind: 'dropped', count: num(data['count']) };
    }
    default:
      return null;
  }
}

/** Reads the `{pending, grants, mode}` body of GET /api/approval. */
export function parsePendingApprovals(raw: unknown): Approval[] {
  const list = Array.isArray(raw) ? raw : isRecord(raw) ? raw['pending'] : null;
  if (!Array.isArray(list)) return [];
  return list.map((p) => parseApproval(p, nextSyntheticID)).filter((a): a is Approval => a !== null);
}
