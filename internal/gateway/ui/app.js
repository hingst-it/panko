'use strict';
(() => {
  const $ = id => document.getElementById(id);
  let token = '', team = '', session = 0, busy = false;
  const cards = new Map();
  const element = (tag, text) => {
    const node = document.createElement(tag);
    if (text !== undefined) node.textContent = text;
    return node;
  };
  function message(body, fallback) {
    return [body.message || fallback, ...(body.errors || []).map(e =>
      [e.field, e.message].filter(Boolean).join(': '))].filter(Boolean).join('\n');
  }
  async function api(path, method = 'GET') {
    const response = await fetch(path, {
      method, credentials: 'omit', cache: 'no-store', mode: 'same-origin',
      headers: {Authorization: 'Bearer ' + token}
    });
    const body = await response.json();
    if (!response.ok) throw new Error(message(body, 'HTTP ' + response.status));
    return body;
  }
  function base(name) {
    return '/v1/teams/' + encodeURIComponent(team) + '/databases' +
      (name === undefined ? '' : '/' + encodeURIComponent(name));
  }
  function graphSVG(graph) {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', 'Configuration resource dependencies');
    const levels = new Map(graph.nodes.map(n => [n.id, 0]));
    const incoming = new Map(graph.nodes.map(n => [n.id, 0]));
    const next = new Map(graph.nodes.map(n => [n.id, []]));
    // DOT points from a resource to its dependency; draw dependencies above dependants.
    for (const [from, to] of graph.edges) {
      next.get(to).push(from);
      incoming.set(from, incoming.get(from) + 1);
    }
    const queue = graph.nodes.filter(n => incoming.get(n.id) === 0).map(n => n.id);
    const visited = new Set();
    while (queue.length) {
      const id = queue.shift();
      visited.add(id);
      for (const child of next.get(id)) {
        levels.set(child, Math.max(levels.get(child), levels.get(id) + 1));
        incoming.set(child, incoming.get(child) - 1);
        if (incoming.get(child) === 0) queue.push(child);
      }
    }
    // Configuration cycles still render in a final bounded layer.
    const last = Math.max(0, ...levels.values()) + 1;
    for (const node of graph.nodes) if (!visited.has(node.id)) levels.set(node.id, last);
    const rows = new Map(), positions = new Map();
    for (const node of graph.nodes) {
      const level = levels.get(node.id);
      if (!rows.has(level)) rows.set(level, []);
      rows.get(level).push(node);
    }
    const width = Math.max(1, ...Array.from(rows.values(), row => row.length)) * 320;
    const height = (Math.max(0, ...levels.values()) + 1) * 90;
    svg.setAttribute('viewBox', '0 0 ' + width + ' ' + height);
    svg.setAttribute('width', width);
    svg.setAttribute('height', height);
    for (const [level, row] of rows) row.forEach((node, i) =>
      positions.set(node.id, [i * 320 + 10, level * 90 + 10]));
    for (const [from, to] of graph.edges) {
      const a = positions.get(from), b = positions.get(to);
      const line = document.createElementNS(ns, 'line');
      line.setAttribute('x1', a[0] + 145);
      line.setAttribute('y1', a[1]);
      line.setAttribute('x2', b[0] + 145);
      line.setAttribute('y2', b[1] + 45);
      svg.append(line);
    }
    for (const node of graph.nodes) {
      const [x, y] = positions.get(node.id);
      const rect = document.createElementNS(ns, 'rect');
      for (const [key, value] of Object.entries({x, y, width: 290, height: 45, rx: 5}))
        rect.setAttribute(key, value);
      const text = document.createElementNS(ns, 'text');
      text.setAttribute('x', x + 8);
      text.setAttribute('y', y + 27);
      text.textContent = node.label;
      const title = document.createElementNS(ns, 'title');
      title.textContent = node.kind + ': ' + node.label;
      svg.append(rect, text, title);
    }
    return svg;
  }
  function card(name) {
    const root = element('article'), heading = element('h2', name);
    const tabs = element('div'), inspector = element('div'), graph = element('div');
    const error = element('p');
    error.setAttribute('role', 'alert');
    graph.hidden = true;
    const inspectButton = element('button', 'Inspector'), graphButton = element('button', 'Graph');
    let graphOpen = false, graphBusy = false;
    const loadGraph = async () => {
      if (!graphOpen || graphBusy) return;
      graphBusy = true;
      const epoch = session;
      try {
        const data = await api(base(name) + '/graph');
        if (epoch !== session) return;
        graph.replaceChildren(element('p', 'Generated: ' + data.generatedAt +
          (data.moduleRevision ? ' · Module: ' + data.moduleRevision : '')), graphSVG(data));
        error.textContent = '';
      } catch (e) {
        if (epoch === session) error.textContent = e.message;
      } finally { graphBusy = false; }
    };
    inspectButton.onclick = () => { graphOpen = false; inspector.hidden = false; graph.hidden = true; error.textContent = ''; };
    graphButton.onclick = () => { graphOpen = true; inspector.hidden = true; graph.hidden = false; loadGraph(); };
    tabs.append(inspectButton, graphButton);
    const details = element('dl'), actions = element('div');
    for (const [label, action] of [['Retry', 'retry'], ['Check drift', 'plan']]) {
      const button = element('button', label);
      button.onclick = async () => {
        const epoch = session;
        button.disabled = true;
        error.textContent = '';
        try {
          await api(base(name) + '/' + action, 'POST');
          if (epoch === session) await refresh();
        } catch (e) { if (epoch === session) error.textContent = e.message; }
        finally { button.disabled = false; }
      };
      actions.append(button);
    }
    inspector.append(details, actions);
    root.append(heading, tabs, inspector, graph, error);
    return {root, update(resource) {
      const s = resource.status || {}, execution = s.lastExecution || {}, plan = s.lastPlan || {};
      details.replaceChildren();
      for (const [key, value] of Object.entries({
        Phase: s.phase, Message: s.message, 'Execution code': execution.code,
        Attempt: execution.attempt, 'Execution finished': execution.finishedAt,
        Changes: execution.changes, Drift: plan.drift, 'Plan finished': plan.finishedAt,
        'State backend': (s.stateBackend || {}).type,
        'Module revision': s.lastAppliedRevision || execution.moduleRevision || s.observedRevision
      })) details.append(element('dt', key), element('dd',
        value === undefined ? '—' : typeof value === 'object' ? JSON.stringify(value) : String(value)));
      loadGraph();
    }};
  }
  async function refresh() {
    if (!token || busy) return;
    busy = true;
    const epoch = session;
    try {
      const resources = await api(base());
      if (epoch !== session) return;
      $('error').textContent = '';
      const names = new Set(resources.map(r => r.name));
      for (const [name, item] of cards) if (!names.has(name)) { item.root.remove(); cards.delete(name); }
      for (const resource of resources) {
        if (!cards.has(resource.name)) {
          const item = card(resource.name);
          cards.set(resource.name, item);
          $('resources').append(item.root);
        }
        cards.get(resource.name).update(resource);
      }
      if (!resources.length) $('error').textContent = 'No Databases in this team.';
    } catch (e) { if (epoch === session) $('error').textContent = e.message; }
    finally { busy = false; }
  }
  function clear() {
    session++;
    token = ''; team = '';
    $('token').value = '';
    cards.clear(); $('resources').replaceChildren(); $('error').textContent = '';
  }
  $('login').addEventListener('submit', event => {
    event.preventDefault();
    const value = $('token').value;
    clear();
    token = value; team = $('team').value.trim();
    refresh();
  });
  $('disconnect').onclick = clear;
  window.addEventListener('pagehide', clear);
  setInterval(refresh, 5000);
})();
