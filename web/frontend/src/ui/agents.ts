// Agent monitor (§26).
//
// The list mirrors the TUI's monitor; selecting a row shows the full detail
// set the spec asks for: task, status, model, current operation, allowed
// tools, modified files, token use, runtime and recent output.

import { el, replaceChildren } from '../util/dom.js';
import { formatDuration, formatNumber, truncate } from '../util/format.js';
import type { AgentView } from '../protocol.js';

const STATUS_CLASS: Record<string, string> = {
  working: 'is-working',
  running: 'is-working',
  testing: 'is-working',
  planning: 'is-working',
  complete: 'is-complete',
  completed: 'is-complete',
  done: 'is-complete',
  waiting: 'is-waiting',
  queued: 'is-waiting',
  idle: 'is-waiting',
  failed: 'is-failed',
  error: 'is-failed',
  cancelled: 'is-failed',
};

function statusClass(status: string): string {
  return STATUS_CLASS[status] ?? 'is-waiting';
}

export class AgentsPanel {
  readonly root: HTMLElement;
  private readonly list: HTMLElement;
  private readonly detail: HTMLElement;
  private selected = '';
  private agents: readonly AgentView[] = [];

  constructor() {
    this.list = el('ul', { class: 'agent-list', aria: { label: 'Agents' } });
    this.detail = el('div', { class: 'agent-detail' });
    this.root = el(
      'section',
      { class: 'panel panel-agents', aria: { label: 'Agents' } },
      el('div', { class: 'agent-columns' }, this.list, this.detail),
    );
  }

  update(agents: readonly AgentView[]): void {
    this.agents = agents;
    if (agents.length === 0) {
      replaceChildren(this.list, [el('li', { class: 'muted', text: 'No agents have been spawned.' })]);
      replaceChildren(this.detail, []);
      return;
    }
    if (!agents.some((a) => a.id === this.selected)) {
      this.selected = (agents[0] as AgentView).id;
    }
    replaceChildren(
      this.list,
      agents.map((agent) => this.row(agent)),
    );
    this.renderDetail();
  }

  private row(agent: AgentView): HTMLElement {
    const isSelected = agent.id === this.selected;
    const button = el(
      'button',
      {
        class: `agent-row ${statusClass(agent.status)}${isSelected ? ' is-selected' : ''}`,
        type: 'button',
        aria: { pressed: isSelected },
        on: {
          click: () => {
            this.selected = agent.id;
            this.update(this.agents);
          },
        },
      },
      el('span', { class: 'agent-dot', aria: { hidden: 'true' } }),
      el('span', { class: 'agent-name', text: agent.name || agent.id }),
      el('span', { class: 'agent-status', text: agent.status.toUpperCase() }),
    );
    return el('li', {}, button);
  }

  private renderDetail(): void {
    const agent = this.agents.find((a) => a.id === this.selected);
    if (!agent) {
      replaceChildren(this.detail, []);
      return;
    }
    const dl = el('dl', { class: 'kv' });
    const add = (label: string, value: string): void => {
      if (value === '') return;
      dl.appendChild(el('dt', { text: label }));
      dl.appendChild(el('dd', { text: value }));
    };
    add('Task', agent.task);
    add('Status', agent.status.toUpperCase());
    add('Model', agent.model);
    add('Current operation', agent.operation);
    add('Allowed tools', agent.tools.join(', '));
    add('Modified files', agent.files.join('\n'));
    add('Token use', agent.tokens > 0 ? formatNumber(agent.tokens) : '');
    add('Runtime', formatDuration(agent.runtimeMs));

    const children: HTMLElement[] = [el('h3', { class: 'panel-title', text: agent.name || agent.id }), dl];
    if (agent.output !== '') {
      children.push(el('h4', { class: 'panel-subtitle', text: 'Recent output' }));
      children.push(el('pre', { class: 'stream' }, document.createTextNode(agent.output)));
    }
    replaceChildren(this.detail, children);
  }
}

export { statusClass, truncate };
