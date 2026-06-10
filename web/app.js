(() => {
  // State
  let activeID = null;
  let evtSource = null;
  let lastEventID = 0;
  let pendingPerm = null;
  let currentAssistantBubble = null;
  const toolChips = new Map();

  // Helpers
  const $ = (id) => document.getElementById(id);
  const truncate = (s, n) => { s = String(s || ""); return s.length > n ? s.slice(0, n) + "..." : s; };
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  function logAppend(node, indent) {
    if (indent) node.classList.add("indented");
    $("log").appendChild(node);
    $("log").scrollTop = $("log").scrollHeight;
  }
  function sysLine(text) {
    const d = document.createElement("div");
    d.className = "bubble sys";
    d.textContent = text;
    logAppend(d, false);
  }

  // REST
  async function listSessions() {
    const r = await fetch("/v1/sessions");
    const data = await r.json();
    renderSessions(data.sessions || []);
  }
  function renderSessions(list) {
    const el = $("session-list");
    el.innerHTML = "";
    if (!list.length) { el.innerHTML = '<div style="color:#888;font-size:12px;">no sessions</div>'; return; }
    for (const s of list) {
      const d = document.createElement("div");
      d.className = "session" + (s.id === activeID ? " active" : "");
      d.innerHTML = `<div><span class="id">${esc(s.id.slice(-10))}</span><span class="status">${esc(s.status)}</span></div>
        <div style="color:#666;font-size:11px;margin-top:2px;">${esc(s.agent_type || "default")} - ${esc(s.provider)}/${esc(s.model)}</div>`;
      d.onclick = () => activate(s.id);
      el.appendChild(d);
    }
  }

  async function createSession() {
    const body = {
      workdir: $("f-workdir").value.trim(),
      provider: $("f-provider").value,
      model: $("f-model").value.trim(),
      ephemeral: $("f-ephemeral").checked,
    };
    const r = await fetch("/v1/sessions", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    if (r.status !== 201) { alert("create failed: " + r.status + " " + await r.text()); return; }
    const s = await r.json();
    await listSessions();
    activate(s.id);
  }

  async function deleteSession() {
    if (!activeID) return;
    if (!confirm("Delete session " + activeID + "?")) return;
    await fetch("/v1/sessions/" + activeID, { method: "DELETE" });
    if (evtSource) { evtSource.close(); evtSource = null; }
    activeID = null;
    lastEventID = 0;
    $("log").innerHTML = "";
    updateToolbar();
    listSessions();
  }
  async function cancelSession() {
    if (!activeID) return;
    await fetch("/v1/sessions/" + activeID + "/cancel", { method: "POST" });
  }

  // Send user message via POST /stream
  async function sendMessage() {
    if (!activeID) return;
    const content = $("msg").value;
    if (!content.trim()) return;
    const payload = { type: "user_message", content };
    const r = await fetch("/v1/sessions/" + activeID + "/stream", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
    if (r.status !== 202) { alert("send failed: " + r.status + " " + await r.text()); return; }
    const d = document.createElement("div"); d.className = "bubble user"; d.textContent = content; logAppend(d, false);
    $("msg").value = "";
  }

  async function sendPermissionDecision(reqID, decision) {
    if (!activeID) return;
    const payload = { type: "permission_decision", request_id: reqID, decision };
    await fetch("/v1/sessions/" + activeID + "/stream", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
  }

  // SSE
  function activate(id) {
    if (evtSource) { evtSource.close(); evtSource = null; }
    activeID = id;
    lastEventID = 0;
    currentAssistantBubble = null;
    toolChips.clear();
    $("log").innerHTML = "";
    $("last-id").textContent = "0";
    sysLine("connected to session " + id);
    evtSource = new EventSource("/v1/sessions/" + id + "/stream");
    for (const t of ["assistant_text_delta", "assistant_text_done", "tool_call_start", "tool_call_done", "permission_request", "permission_resolved", "subagent_event", "compaction", "provider_retry", "error", "interrupted", "pong"]) {
      evtSource.addEventListener(t, (e) => handleEvent(t, e));
    }
    evtSource.onerror = () => sysLine("[stream interrupted, will auto-reconnect with Last-Event-ID=" + lastEventID + "]");
    updateToolbar();
    listSessions();
  }

  function handleEvent(type, e) {
    if (e.lastEventId) { lastEventID = parseInt(e.lastEventId, 10); $("last-id").textContent = String(lastEventID); }
    let payload = {}; try { payload = JSON.parse(e.data); } catch (_) {}
    const subagent = payload.subagent_id || payload.parent_id;
    switch (type) {
      case "assistant_text_delta":
        if (!currentAssistantBubble) { currentAssistantBubble = document.createElement("div"); currentAssistantBubble.className = "bubble assistant"; logAppend(currentAssistantBubble, !!subagent); }
        currentAssistantBubble.textContent += payload.delta || "";
        $("log").scrollTop = $("log").scrollHeight;
        break;
      case "assistant_text_done":
        currentAssistantBubble = null;
        break;
      case "tool_call_start": {
        const chip = document.createElement("div"); chip.className = "chip tool";
        chip.textContent = (payload.tool_name || "tool") + " <- " + truncate(JSON.stringify(payload.input || {}), 80);
        logAppend(chip, !!subagent);
        if (payload.tool_use_id) toolChips.set(payload.tool_use_id, chip);
        break;
      }
      case "tool_call_done": {
        const chip = toolChips.get(payload.tool_use_id);
        const out = truncate(payload.output || "", 120);
        const txt = (payload.tool_name || "tool") + " -> " + out;
        if (chip) { chip.textContent = txt; chip.classList.add(payload.is_error ? "error" : "done"); }
        else { const c = document.createElement("div"); c.className = "chip tool " + (payload.is_error ? "error" : "done"); c.textContent = txt; logAppend(c, !!subagent); }
        toolChips.delete(payload.tool_use_id);
        break;
      }
      case "permission_request":
        pendingPerm = payload;
        $("perm-summary").textContent = (payload.tool_name || "tool") + " requests permission";
        $("perm-detail").textContent = JSON.stringify(payload, null, 2);
        $("perm-modal").classList.add("show");
        break;
      case "permission_resolved":
        if (pendingPerm && payload.request_id === pendingPerm.request_id) {
          pendingPerm = null;
          $("perm-modal").classList.remove("show");
        }
        sysLine("[permission " + (payload.tool || "?") + ": " + (payload.decision || "?") + " via " + (payload.source || "?") + "]");
        break;
      case "subagent_event": {
        const inner = payload.event || {};
        const c = document.createElement("div"); c.className = "bubble sys"; c.textContent = "[subagent " + truncate(payload.subagent_id || "", 10) + "] " + (inner.type || "?");
        logAppend(c, true);
        break;
      }
      case "compaction":
        sysLine("[compaction: " + (payload.from_tokens || 0) + " -> " + (payload.to_tokens || 0) + " tokens]");
        break;
      case "provider_retry":
        sysLine("[provider retry #" + (payload.attempt || 0) + ": " + (payload.reason || "") + "]");
        break;
      case "error":
        sysLine("[error " + (payload.code || "?") + ": " + (payload.message || "") + "]");
        break;
      case "interrupted":
        sysLine("[interrupted]");
        currentAssistantBubble = null;
        break;
      case "pong":
        break;
    }
  }

  function updateToolbar() {
    const has = !!activeID;
    $("btn-delete").disabled = !has;
    $("btn-cancel").disabled = !has;
    $("msg").disabled = !has;
    $("btn-send").disabled = !has;
    $("active-info").textContent = has ? "active: " + activeID : "No session selected";
  }

  // Wire UI
  $("btn-create").onclick = createSession;
  $("btn-refresh").onclick = listSessions;
  $("btn-delete").onclick = deleteSession;
  $("btn-cancel").onclick = cancelSession;
  $("btn-send").onclick = sendMessage;
  $("msg").addEventListener("keydown", (e) => { if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); sendMessage(); } });
  document.querySelectorAll("#perm-modal button[data-decision]").forEach((b) => {
    b.onclick = () => {
      const d = b.getAttribute("data-decision");
      if (pendingPerm) sendPermissionDecision(pendingPerm.request_id, d);
      pendingPerm = null;
      $("perm-modal").classList.remove("show");
    };
  });

  // Permissions panel
  async function listPermissions() {
    try {
      const r = await fetch("/v1/permissions");
      const data = await r.json();
      renderPermissions(data.rules || []);
    } catch (e) {
      $("perm-list").innerHTML = '<div style="color:#c53030;">' + esc(String(e)) + '</div>';
    }
  }
  function renderPermissions(rules) {
    const el = $("perm-list");
    if (!rules.length) { el.innerHTML = '<div style="color:#888;font-size:11px;">no rules</div>'; return; }
    let html = '';
    for (const r of rules) {
      const tool = r.tool || '*';
      const pat = r.pattern || '**';
      const d = r.decision || '';
      const src = r.source || 'manual';
      html += '<div style="border-bottom:1px solid #f0f0f0;padding:4px 0;display:flex;align-items:center;gap:4px;">' +
        '<span style="flex:1;"><b>' + esc(tool) + '</b> ' + esc(pat) + '</span>' +
        '<span style="font-size:10px;color:#666;white-space:nowrap;">' + esc(d.replace('allow_','').replace('_permanent','')) + ' (' + esc(src[0]) + ')' + '</span>' +
        '<button class="perm-del" data-id="' + esc(r.id) + '" data-source="' + esc(src) + '" style="background:#c53030;border:none;color:#fff;padding:1px 6px;border-radius:3px;cursor:pointer;font-size:10px;">x</button>' +
        '</div>';
    }
    el.innerHTML = html;
    el.querySelectorAll(".perm-del").forEach((b) => {
      b.onclick = async () => {
        const id = b.getAttribute("data-id");
        if (b.getAttribute("data-source") === "preset" &&
            !confirm("This is a preset rule. Deleting it now only lasts until the next server restart, when config tools.preset_rules re-injects it. Delete anyway?")) {
          return;
        }
        try {
          const r = await fetch("/v1/permissions/" + id, { method: "DELETE" });
          if (r.status === 204) listPermissions();
          else alert("delete failed: " + r.status + " " + await r.text());
        } catch (e) { alert(String(e)); }
      };
    });
  }
  async function addPermission() {
    const tool = $("perm-tool").value;
    const pattern = $("perm-pattern").value.trim();
    const decision = $("perm-decision").value;
    try {
      const r = await fetch("/v1/permissions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ tool, pattern, decision })
      });
      if (r.status === 201) {
        $("perm-pattern").value = "";
        listPermissions();
      } else {
        const err = await r.text();
        alert("add failed: " + r.status + " " + err);
      }
    } catch (e) { alert(String(e)); }
  }
  $("btn-perm-refresh").onclick = listPermissions;
  $("btn-perm-add").onclick = addPermission;

  listSessions();
  listPermissions();
})();
