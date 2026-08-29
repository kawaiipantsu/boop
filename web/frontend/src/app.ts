// Wiring. Everything above this file is independently testable; this is the
// only place that knows about all of it at once.

import './styles.css';

import { api, ApiError } from './api.js';
import { EventSocket, type ConnectionState } from './socket.js';
import { Store } from './store.js';
import type { ApprovalEvent, BoopEvent, GrantScope, HelloData } from './protocol.js';
import { el } from './util/dom.js';
import { ApprovalPanel } from './ui/approvals.js';
import { AgentsPanel } from './ui/agents.js';
import { Composer } from './ui/composer.js';
import { Header } from './ui/header.js';
import { ModelsPanel, SessionsPanel, SettingsPanel, ToolsPanel } from './ui/panels.js';
import { Shell, type View } from './ui/shell.js';
import { StatsPanel } from './ui/stats.js';
import { Transcript } from './ui/transcript.js';

function message(err: unknown): string {
  if (err instanceof ApiError) return err.status === 0 ? `Cannot reach Boop: ${err.message}` : err.message;
  return err instanceof Error ? err.message : String(err);
}

export function mount(root: HTMLElement): () => void {
  const store = new Store();
  const transcript = new Transcript();

  const socket = new EventSocket({
    onEvent: (ev: BoopEvent) => {
      store.apply(ev);
      transcript.handle(ev);
    },
    onState: (state: ConnectionState, detail) => store.setConnection(state, detail.nextRetryMs),
    onHello: (hello: HelloData) => store.applyHello(hello),
    onApproval: (ev: ApprovalEvent) => store.applyApprovalEvent(ev),
    onServerError: (text: string) => {
      store.setError(text);
      transcript.addNotice(text, 'error');
    },
    onDropped: (count: number) => {
      transcript.addNotice(
        `This page fell behind and the server dropped ${count} update(s). Resyncing.`,
        'error',
      );
      void resync();
    },
    onOpen: () => {
      // The socket may have been down while things happened. `hello` carries
      // the session and approval queue; the rest comes back over REST.
      void resync();
    },
  });

  const approvals = new ApprovalPanel({
    resolve: async (id: string, approved: boolean, scope: GrantScope) => {
      // The socket carries approvals when it is up — that is the whole reason
      // §24 chose a WebSocket. The server broadcasts the resolution back, but
      // we clear locally too so the dialog never lingers on a slow round trip.
      if (socket.send('approval', { id, approved, scope })) {
        store.clearApproval(id);
        return;
      }
      try {
        await api.resolveApproval(id, approved, scope);
        store.clearApproval(id);
      } catch (err) {
        store.setError(message(err));
        transcript.addNotice(`Could not send the approval decision: ${message(err)}`, 'error');
      }
    },
  });

  const header = new Header(() => socket.retryNow());

  async function interrupt(): Promise<void> {
    const sessionId = store.get().status.sessionId;
    // web.ClientCancel is the real mechanism; REST is only for a dead socket.
    const sent = socket.send('cancel', sessionId ? { session_id: sessionId } : {});
    if (!sent) {
      try {
        await api.interrupt(sessionId || undefined);
      } catch (err) {
        transcript.addNotice(`Could not send the interrupt: ${message(err)}`, 'error');
        return;
      }
    }
    transcript.addNotice('Interrupt sent.');
  }

  async function newSession(): Promise<void> {
    try {
      const id = await api.newSession();
      transcript.clear();
      store.setStatus({ ...store.get().status, sessionId: id });
      store.setBusy(false);
      transcript.addNotice('Started a new session.');
    } catch (err) {
      transcript.addNotice(`Could not start a new session: ${message(err)}`, 'error');
    }
  }

  const composer = new Composer({
    submit: async (text: string) => {
      transcript.addUserTurn(text);
      store.setBusy(true);
      const sessionId = store.get().status.sessionId;
      // Over the socket a turn is always asynchronous, which is what we want:
      // the answer is the event stream, not an HTTP response body.
      const data: Record<string, unknown> = { content: text };
      if (sessionId) data['session_id'] = sessionId;
      if (socket.send('message', data)) return;
      try {
        const result = await api.sendMessage(text, sessionId || undefined);
        if (result.sessionId) store.setStatus({ ...store.get().status, sessionId: result.sessionId });
      } catch (err) {
        store.setBusy(false);
        composer.setError(message(err));
        transcript.addNotice(`Message not delivered: ${message(err)}`, 'error');
      }
    },
    interrupt,
    newSession,
  });

  const agentsPanel = new AgentsPanel();
  const toolsPanel = new ToolsPanel(() => refreshTools());
  const statsPanel = new StatsPanel(() => refreshStats());
  const modelsPanel = new ModelsPanel(() => refreshModels());
  const sessionsPanel = new SessionsPanel(
    () => refreshSessions(),
    () => newSession(),
  );
  const settingsPanel = new SettingsPanel(
    () => refreshConfig(),
    async (config) => {
      await api.saveConfig(config);
      await refreshStatus();
    },
  );

  const chat = el(
    'section',
    { class: 'panel panel-chat', aria: { label: 'Chat' } },
    transcript.root,
    composer.root,
  );

  const views: View[] = [
    { id: 'chat', label: 'Chat', node: chat, onShow: () => composer.focus() },
    { id: 'agents', label: 'Agents', node: agentsPanel.root, onShow: () => void refreshAgents() },
    { id: 'tools', label: 'Tools', node: toolsPanel.root, onShow: () => void refreshTools() },
    { id: 'models', label: 'Models', node: modelsPanel.root, onShow: () => void refreshModels() },
    { id: 'sessions', label: 'Sessions', node: sessionsPanel.root, onShow: () => void refreshSessions() },
    { id: 'stats', label: 'Statistics', node: statsPanel.root, onShow: () => void refreshStats() },
    { id: 'settings', label: 'Settings', node: settingsPanel.root, onShow: () => void refreshConfig() },
  ];

  const shell = new Shell(header.root, approvals.root, views);
  root.replaceChildren(shell.root);

  const unsubscribe = store.subscribe((state) => {
    header.update(state);
    composer.update(state);
    approvals.render(state.approvals);
    agentsPanel.update(state.agents);
  });

  async function refreshStatus(): Promise<void> {
    try {
      store.setStatus(await api.status());
    } catch (err) {
      store.setError(message(err));
    }
  }
  async function refreshApprovals(): Promise<void> {
    try {
      store.setApprovals(await api.approvals());
    } catch {
      /* the hello frame is the primary source; this is only a backstop */
    }
  }
  async function refreshTools(): Promise<void> {
    try {
      toolsPanel.render(await api.tools());
    } catch (err) {
      toolsPanel.setError(message(err));
    }
  }
  /** Re-reads everything the socket cannot replay. */
  async function resync(): Promise<void> {
    await Promise.all([refreshStatus(), refreshAgents(), refreshApprovals()]);
  }
  async function refreshAgents(): Promise<void> {
    try {
      store.setAgents(await api.agents());
    } catch {
      /* the agents endpoint may not exist yet; the panel stays as it is */
    }
  }
  async function refreshStats(): Promise<void> {
    try {
      statsPanel.render(await api.stats());
    } catch (err) {
      statsPanel.setError(message(err));
    }
  }
  async function refreshModels(): Promise<void> {
    try {
      const [providers, models] = await Promise.all([api.providers(), api.models()]);
      modelsPanel.render(providers, models);
    } catch (err) {
      modelsPanel.setError(message(err));
    }
  }
  async function refreshSessions(): Promise<void> {
    try {
      sessionsPanel.render(await api.sessions(), store.get().status.sessionId);
    } catch (err) {
      sessionsPanel.setError(message(err));
    }
  }
  async function refreshConfig(): Promise<void> {
    try {
      settingsPanel.render(await api.config());
    } catch (err) {
      settingsPanel.setError(message(err));
    }
  }

  const onOnline = (): void => socket.retryNow();
  const onVisible = (): void => {
    if (document.visibilityState === 'visible') socket.retryNow();
  };
  const onKeyDown = (ev: KeyboardEvent): void => {
    // §51: an interrupt must be reachable without the mouse. Escape while the
    // composer has focus cancels the running operation.
    if (ev.key === 'Escape' && store.get().busy && !approvals.root.contains(document.activeElement)) {
      ev.preventDefault();
      void interrupt();
    }
  };

  window.addEventListener('online', onOnline);
  document.addEventListener('visibilitychange', onVisible);
  document.addEventListener('keydown', onKeyDown);

  socket.start();
  void resync();

  return () => {
    window.removeEventListener('online', onOnline);
    document.removeEventListener('visibilitychange', onVisible);
    document.removeEventListener('keydown', onKeyDown);
    socket.stop();
    unsubscribe();
  };
}
