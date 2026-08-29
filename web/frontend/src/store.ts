// Application state.
//
// Everything that several views need to agree on lives here: connection state,
// header status, the agent roster, the approval queue and whether a turn is in
// flight. Views subscribe and re-render themselves.
//
// The transcript is deliberately NOT in here. It is append-only, high-frequency
// DOM, and putting it behind a subscribe/re-render cycle would mean rebuilding
// the conversation on every streamed token.

import type { ConnectionState } from './socket.js';
import {
  durationMs, isRecord, num, parseAgent, parseApproval, str,
  type AgentView, type Approval, type BoopEvent, type StatusView,
} from './protocol.js';

export interface AppState {
  connection: ConnectionState;
  /** Milliseconds until the next reconnect attempt; 0 when connected. */
  nextRetryMs: number;
  status: StatusView;
  agents: AgentView[];
  approvals: Approval[];
  /** True between prompt.received and session.completed / error. */
  busy: boolean;
  lastError: string;
}

export function emptyStatus(): StatusView {
  return {
    version: '',
    provider: '',
    model: '',
    mode: '',
    sessionId: '',
    projectPath: '',
    agents: 0,
    tokens: { prompt: 0, completion: 0, total: 0 },
    pendingApprovals: [],
  };
}

type Listener = (state: AppState) => void;

export class Store {
  private state: AppState = {
    connection: 'connecting',
    nextRetryMs: 0,
    status: emptyStatus(),
    agents: [],
    approvals: [],
    busy: false,
    lastError: '',
  };

  private readonly listeners = new Set<Listener>();
  private synthetic = 0;

  get(): Readonly<AppState> {
    return this.state;
  }

  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    fn(this.state);
    return () => this.listeners.delete(fn);
  }

  private patch(next: Partial<AppState>): void {
    this.state = { ...this.state, ...next };
    for (const fn of this.listeners) fn(this.state);
  }

  setConnection(connection: ConnectionState, nextRetryMs = 0): void {
    if (this.state.connection === connection && this.state.nextRetryMs === nextRetryMs) return;
    this.patch({ connection, nextRetryMs });
  }

  setStatus(status: StatusView): void {
    // A REST poll must not clobber token counts we have already streamed past,
    // nor drop approvals that arrived on the socket while the poll was in
    // flight.
    const merged: StatusView = {
      ...status,
      tokens:
        status.tokens.total >= this.state.status.tokens.total ? status.tokens : this.state.status.tokens,
      pendingApprovals: [],
    };
    const known = new Set(this.state.approvals.map((a) => a.id));
    const extra = status.pendingApprovals.filter((a) => !known.has(a.id));
    this.patch({
      status: merged,
      approvals: extra.length > 0 ? [...this.state.approvals, ...extra] : this.state.approvals,
    });
  }

  setAgents(agents: AgentView[]): void {
    this.patch({ agents, status: { ...this.state.status, agents: agents.length } });
  }

  setBusy(busy: boolean): void {
    if (this.state.busy !== busy) this.patch({ busy });
  }

  setError(message: string): void {
    this.patch({ lastError: message });
  }

  clearApproval(id: string): void {
    if (!this.state.approvals.some((a) => a.id === id)) return;
    this.patch({ approvals: this.state.approvals.filter((a) => a.id !== id) });
  }

  addApproval(approval: Approval): void {
    if (this.state.approvals.some((a) => a.id === approval.id)) return;
    this.patch({ approvals: [...this.state.approvals, approval] });
  }

  private upsertAgent(partial: AgentView): void {
    const idx = this.state.agents.findIndex((a) => a.id === partial.id);
    const agents = [...this.state.agents];
    if (idx === -1) {
      agents.push(partial);
    } else {
      const prev = agents[idx] as AgentView;
      agents[idx] = {
        ...prev,
        ...Object.fromEntries(
          Object.entries(partial).filter(([, v]) => {
            if (v === '' || v === null) return false;
            if (Array.isArray(v) && v.length === 0) return false;
            if (typeof v === 'number' && v === 0) return false;
            return true;
          }),
        ),
        id: prev.id,
      } as AgentView;
    }
    this.patch({ agents, status: { ...this.state.status, agents: agents.length } });
  }

  /** Folds one bus event into state. Unknown event types are ignored. */
  apply(ev: BoopEvent): void {
    switch (ev.type) {
      case 'session.started': {
        const sid = ev.session_id || (isRecord(ev.payload) ? str(ev.payload['id']) : '');
        if (sid) this.patch({ status: { ...this.state.status, sessionId: sid } });
        break;
      }
      case 'prompt.received':
        this.patch({ busy: true, lastError: '' });
        break;
      case 'model.request.started':
        this.setBusy(true);
        break;
      case 'model.response.completed': {
        if (isRecord(ev.payload)) {
          const usage = isRecord(ev.payload['usage']) ? ev.payload['usage'] : ev.payload;
          const prompt = num(usage['prompt_tokens']);
          const completion = num(usage['completion_tokens']);
          const total = num(usage['total_tokens'], prompt + completion);
          if (total > 0) {
            const t = this.state.status.tokens;
            this.patch({
              status: {
                ...this.state.status,
                tokens: {
                  prompt: t.prompt + prompt,
                  completion: t.completion + completion,
                  total: t.total + total,
                },
              },
            });
          }
        }
        break;
      }
      case 'session.completed':
        this.setBusy(false);
        break;
      case 'agent.created':
      case 'agent.status.changed': {
        const parsed = parseAgent(
          isRecord(ev.payload) && ev.payload['id'] === undefined && ev.agent_id
            ? { ...ev.payload, id: ev.agent_id }
            : (ev.payload ?? (ev.agent_id ? { id: ev.agent_id } : null)),
        );
        if (parsed) this.upsertAgent(parsed);
        break;
      }
      case 'approval.requested': {
        const approval = parseApproval(ev.payload, () => {
          this.synthetic += 1;
          return `pending-${this.synthetic}`;
        });
        if (approval) {
          if (!approval.requestedAt && ev.at) approval.requestedAt = ev.at;
          this.addApproval(approval);
        }
        break;
      }
      case 'approval.received': {
        // The core resolves an approval when any frontend answers it, so this
        // is how a second browser tab (or the TUI) clears our dialog (§50).
        const rec = isRecord(ev.payload) ? ev.payload : {};
        const id = str(rec['id']);
        if (id !== '') {
          this.clearApproval(id);
        } else {
          const tool = str(rec['tool']);
          const match = this.state.approvals.find((a) => a.action.tool === tool);
          if (match) this.clearApproval(match.id);
          else if (this.state.approvals.length > 0) {
            this.clearApproval((this.state.approvals[0] as Approval).id);
          }
        }
        break;
      }
      case 'error':
        this.setBusy(false);
        break;
      default:
        break;
    }
  }
}

/** Runtime for one agent row when the server reports a start time instead. */
export function agentRuntimeMs(agent: AgentView, startedAt?: string, now = Date.now()): number | null {
  if (agent.runtimeMs !== null) return agent.runtimeMs;
  if (!startedAt) return null;
  const t = Date.parse(startedAt);
  return Number.isNaN(t) ? null : now - t;
}

export { durationMs };
