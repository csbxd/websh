"use strict";

(() => {
  const $ = (id) => document.getElementById(id);
  const root = $("websh");
  const state = { authenticated: false, info: null, sessions: [], session: null, activeID: "", pending: false, creating: false, inputPending: false, pollTimer: null, pollInFlight: false, connected: false };
  const cards = new Map();
  const stateLabels = { idle: "空闲", running: "执行中", closed: "已关闭" };
  const commandLabels = { running: "执行中", completed: "已完成", interrupted: "已中断", timed_out: "已超时", failed: "执行失败" };
  const storageKey = "websh.uncertain-submissions.v1";
  let uncertain = {};
  try { uncertain = JSON.parse(localStorage.getItem(storageKey) || "{}"); } catch (_) { /* Storage can be unavailable. */ }
  if (!uncertain || typeof uncertain !== "object" || Array.isArray(uncertain)) uncertain = {};

  class APIError extends Error {
    constructor(message, status = 0) { super(message); this.status = status; }
  }

  function setText(element, text) {
    if (element.textContent !== text) element.textContent = text;
  }

  function persistUncertain() {
    try { localStorage.setItem(storageKey, JSON.stringify(uncertain)); } catch (_) { /* In-memory protection still applies. */ }
  }

  function setConnected(connected) {
    state.connected = connected;
    root.dataset.connected = String(connected);
    setText($("connection-status"), connected ? "● 已连接" : "● 连接中断，自动重连");
    updateStateJSON();
  }

  function showNotice(message, kind = "") {
    const notice = $("notice");
    notice.textContent = message;
    notice.className = `notice ${kind}`;
    notice.hidden = !message;
  }

  function setAuthenticated(authenticated) {
    state.authenticated = authenticated;
    root.dataset.authenticated = String(authenticated);
    $("login-panel").hidden = authenticated;
    $("workspace").hidden = !authenticated;
    if (!authenticated) clearTimeout(state.pollTimer);
    updateStateJSON();
  }

  async function api(path, options = {}) {
    let response;
    const controller = new AbortController();
    const requestTimeout = setTimeout(() => controller.abort(), 10000);
    try {
      try {
        response = await fetch(path, {
          ...options,
          signal: controller.signal,
          credentials: "same-origin",
          cache: "no-store",
          headers: { "Content-Type": "application/json", ...options.headers },
          body: options.body === undefined ? undefined : JSON.stringify(options.body),
        });
      } catch (_) {
        setConnected(false);
        throw new APIError(controller.signal.aborted ? "服务器响应超时，请检查连接状态。" : "无法连接服务器。请检查 WebSH 是否仍在运行。");
      }
      setConnected(true);
      let body;
      try { body = await response.json(); }
      catch (_) {
        if (response.ok) { setConnected(false); throw new APIError("未收到完整的服务器响应。"); }
        body = null;
      }
      if (!response.ok) {
        if (response.status === 401 && path !== "/api/login") {
          setAuthenticated(false);
          showNotice("登录已失效，请重新输入访问令牌。", "error");
        }
        throw new APIError(body?.error || `请求失败（HTTP ${response.status}）`, response.status);
      }
      return body;
    } finally {
      clearTimeout(requestTimeout);
    }
  }

  function schedulePoll(delay) {
    clearTimeout(state.pollTimer);
    if (state.authenticated) state.pollTimer = setTimeout(poll, delay ?? (state.session?.state === "running" ? 300 : 1500));
  }

  async function poll() {
    if (state.pollInFlight || !state.authenticated) { schedulePoll(); return; }
    state.pollInFlight = true;
    try {
      if (state.activeID) {
        const id = state.activeID;
        const session = await api(`/api/sessions/${encodeURIComponent(id)}`);
        if (state.activeID === id) applySession(session);
      } else {
        await loadSessions();
      }
    } catch (error) {
      if (error.status === 404) {
        state.activeID = "";
        state.session = null;
        renderSession();
        try { await loadSessions(); } catch (_) { /* The next poll recovers. */ }
      }
      // Preserve visible command output and typed text while the network recovers.
    } finally {
      state.pollInFlight = false;
      schedulePoll(state.connected ? undefined : 2500);
    }
  }

  function renderSessionPicker() {
    const select = $("session-select");
    const signature = JSON.stringify(state.sessions.map((s) => [s.id, s.name, s.state]));
    if (select.dataset.signature !== signature) {
      select.replaceChildren();
      if (!state.sessions.length) {
        const option = document.createElement("option");
        option.value = "";
        option.textContent = "尚无会话";
        select.append(option);
      }
      for (const session of state.sessions) {
        const option = document.createElement("option");
        option.value = session.id;
        option.textContent = `${session.name || session.id.slice(0, 12)}${session.state === "closed" ? "（已关闭）" : ""}`;
        select.append(option);
      }
      select.dataset.signature = signature;
    }
    select.value = state.activeID;
  }

  async function loadSessions({ autoCreate = false } = {}) {
    const result = await api("/api/sessions");
    state.sessions = Array.isArray(result?.sessions) ? result.sessions : [];
    if (!state.sessions.length && autoCreate && !state.creating) {
      await createSession({});
      return;
    }
    if (!state.sessions.some((s) => s.id === state.activeID)) {
      state.activeID = state.sessions.find((s) => s.state !== "closed")?.id || state.sessions[0]?.id || "";
    }
    renderSessionPicker();
    const session = state.sessions.find((s) => s.id === state.activeID);
    if (session) applySession(session);
    else { state.session = null; renderSession(); }
  }

  function applySession(session) {
    if (!session || session.id !== state.activeID) return;
    state.session = session;
    const index = state.sessions.findIndex((s) => s.id === session.id);
    if (index >= 0) state.sessions[index] = session;
    else state.sessions.push(session);
    const pending = uncertain[session.id];
    if (pending) {
      const knownIDs = new Set(pending.knownIDs || []);
      const accepted = (session.commands || []).find((c) => !knownIDs.has(c.id) && c.command === pending.command);
      if (accepted) {
        delete uncertain[session.id];
        persistUncertain();
        showNotice(`已确认上次提交：命令 ${accepted.id}。`);
      }
    }
    renderSessionPicker();
    renderSession();
  }

  function renderSession() {
    const session = state.session;
    root.dataset.activeSessionId = session?.id || "";
    root.dataset.sessionState = session?.state || "none";
    root.dataset.activeCommandId = session?.active_command_id || "";
    setText($("session-status"), session ? `${stateLabels[session.state] || session.state} · ${session.state}` : "未选择会话");
    setText($("session-id"), session?.id || "—");
    setText($("current-cwd"), session?.cwd || "—");
    setText($("active-command-id"), session?.active_command_id || "—");
    renderCommands(session?.commands || []);
    updateControls();
    updateStateJSON();
  }

  function updateControls() {
    const session = state.session;
    const running = session?.state === "running";
    const locked = !!uncertain[session?.id];
    $("run-command").disabled = !session || session.state !== "idle" || state.pending || locked;
    setText($("run-command"), state.pending ? "正在提交…" : running ? "命令执行中" : "执行命令");
    $("interrupt-command").disabled = !running || state.pending;
    $("close-session").disabled = !session || session.state === "closed" || state.pending;
    $("new-session").disabled = state.creating;
    $("create-session").disabled = state.creating;
    $("stdin-panel").hidden = !running;
    $("send-input").disabled = !running || state.inputPending;
    $("send-eof").disabled = !running || state.inputPending;
    $("submission-uncertain").hidden = !locked || state.pending;
    $("resolve-submission").disabled = !session || session.state !== "idle" || state.pending || !state.connected;
    root.dataset.submissionUncertain = String(locked);
  }

  function node(tag, className, text) {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = text;
    return element;
  }

  function createCard(command, index) {
    const card = node("article", "command-card");
    card.id = `command-${command.id}`;
    card.dataset.commandId = command.id;
    card.setAttribute("aria-labelledby", `command-title-${command.id}`);
    const header = node("div", "command-card-header");
    const titleGroup = node("div");
    const title = node("h3", "command-card-title", `命令 ${index + 1}`);
    title.id = `command-title-${command.id}`;
    const id = node("code", "command-card-id", command.id);
    const status = node("span", "command-status");
    status.dataset.field = "status";
    titleGroup.append(title, id);
    header.append(titleGroup, status);
    const source = node("pre", "command-source", command.command);
    source.dataset.field = "command";
    source.setAttribute("aria-label", "执行的 Shell 命令");
    card.append(header, source);
    const streams = {};
    for (const stream of ["stdout", "stderr"]) {
      const area = node("section", "command-output");
      area.setAttribute("aria-label", stream === "stdout" ? "标准输出" : "标准错误");
      const label = node("div", "stream-label", stream === "stdout" ? "STDOUT · 标准输出" : "STDERR · 标准错误");
      const output = node("pre", "output-pre");
      output.dataset.stream = stream;
      output.id = `${stream}-${command.id}`;
      const empty = node("p", "empty-output", "暂无输出");
      area.append(label, output, empty);
      card.append(area);
      streams[stream] = { output, empty };
    }
    const meta = node("div", "command-card-meta");
    const fields = {};
    for (const [field, label] of [["exit-code", "退出码"], ["cwd", "目录"], ["started-at", "开始"], ["finished-at", "结束"]]) {
      const item = node("span", "", `${label}：`);
      const value = node("strong");
      value.dataset.field = field;
      item.append(value);
      meta.append(item);
      fields[field] = value;
    }
    const warning = node("p", "truncation-warning");
    warning.hidden = true;
    const rawDetails = node("details", "raw-output-details");
    rawDetails.hidden = true;
    const rawSummary = node("summary");
    const rawOutput = node("pre", "raw-output-json");
    rawOutput.id = `raw-output-${command.id}`;
    rawOutput.dataset.field = "raw-output";
    rawDetails.append(rawSummary, rawOutput);
    card.append(meta, warning, rawDetails);
    return { card, title, status, source, streams, fields, warning, rawDetails, rawSummary, rawOutput };
  }

  function readableOutput(text) {
    // Plain DOM output is easier to read than terminal escape sequences. Keep the
    // original strings in an expandable JSON block whenever the display changes.
    return text
      .replace(/\x1b\][\s\S]*?(?:\x07|\x1b\\)/g, "")
      .replace(/\x1b[P_X^][\s\S]*?\x1b\\/g, "")
      .replace(/(?:\x1b\[|\x9b)[0-?]*[ -/]*[@-~]/g, "")
      .replace(/\x1b[ -/]*[@-Z\\-_]/g, "")
      .replace(/\r\n?/g, "\n")
      .replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, (char) => `\\x${char.charCodeAt(0).toString(16).padStart(2, "0")}`);
  }

  function formatTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? value : date.toLocaleTimeString("zh-CN", { hour12: false });
  }

  function renderCommands(commands) {
    const history = $("command-history");
    const visible = new Set(commands.map((c) => c.id));
    for (const [id, elements] of cards) {
      if (!visible.has(id)) { elements.card.remove(); cards.delete(id); }
    }
    commands.forEach((command, index) => {
      let elements = cards.get(command.id);
      if (!elements) {
        elements = createCard(command, index);
        cards.set(command.id, elements);
        history.append(elements.card);
      }
      const { card, status, source, streams, fields, warning, title, rawDetails, rawSummary, rawOutput } = elements;
      setText(title, `命令 ${index + 1}`);
      card.dataset.status = command.status;
      card.dataset.exitCode = command.exit_code == null ? "" : String(command.exit_code);
      card.dataset.cwd = command.cwd || "";
      card.dataset.truncated = String(!!command.truncated);
      setText(status, `${commandLabels[command.status] || command.status} · ${command.status}`);
      if (source.textContent !== command.command) source.textContent = command.command;
      let normalized = false;
      for (const stream of ["stdout", "stderr"]) {
        const original = command[stream] || "";
        const content = readableOutput(original);
        if (content !== original) normalized = true;
        const { output, empty } = streams[stream];
        if (output.textContent !== content) {
          const following = output.scrollHeight - output.scrollTop - output.clientHeight < 35;
          output.textContent = content;
          if (following) output.scrollTop = output.scrollHeight;
        }
        empty.hidden = !!content;
        setText(empty, content ? "" : "暂无输出");
        output.hidden = !content;
      }
      card.dataset.outputNormalized = String(normalized);
      rawDetails.hidden = !normalized;
      setText(rawSummary, normalized ? "显示原始输出 JSON（界面已清理 ANSI / 转义控制字符）" : "");
      if (normalized) {
        const json = JSON.stringify({ stdout: command.stdout || "", stderr: command.stderr || "" }, null, 2);
        if (rawOutput.textContent !== json) rawOutput.textContent = json;
      } else setText(rawOutput, "");
      setText(fields["exit-code"], command.exit_code == null ? "—" : String(command.exit_code));
      setText(fields.cwd, command.cwd || "—");
      setText(fields["started-at"], formatTime(command.started_at));
      fields["started-at"].title = command.started_at || "";
      setText(fields["finished-at"], formatTime(command.finished_at));
      fields["finished-at"].title = command.finished_at || "";
      warning.hidden = !command.truncated;
      setText(warning, command.truncated ? "输出已达到服务器保留上限，命令被中断，部分内容被截断。需要完整输出时，请将命令输出写入文件。" : "");
    });
    setText($("command-count"), `${commands.length} 条命令`);
    $("empty-history").hidden = commands.length > 0;
    root.dataset.commandCount = String(commands.length);
  }

  function updateStateJSON() {
    const session = state.session;
    const snapshot = {
      authenticated: state.authenticated,
      connected: state.connected,
      submission_uncertain: !!uncertain[session?.id],
      session: session ? {
        id: session.id, name: session.name, cwd: session.cwd, state: session.state,
        active_command_id: session.active_command_id || null,
        commands: (session.commands || []).map((c) => ({
          id: c.id, command: c.command, status: c.status, exit_code: c.exit_code ?? null,
          cwd: c.cwd, started_at: c.started_at, finished_at: c.finished_at || null,
          truncated: !!c.truncated,
          stdout_element: `#stdout-${c.id}`, stderr_element: `#stderr-${c.id}`,
          raw_output_element: `#raw-output-${c.id}`,
          output_normalized: readableOutput(c.stdout || "") !== (c.stdout || "") || readableOutput(c.stderr || "") !== (c.stderr || ""),
          stdout_length: (c.stdout || "").length, stderr_length: (c.stderr || "").length,
        })),
      } : null,
    };
    const json = JSON.stringify(snapshot, null, 2);
    if ($("session-state").textContent !== json) $("session-state").textContent = json;
  }

  async function createSession(body) {
    if (state.creating) return;
    state.creating = true;
    updateControls();
    try {
      const session = await api("/api/sessions", { method: "POST", body });
      state.activeID = session.id;
      applySession(session);
      $("new-session-form").hidden = true;
      $("session-name").value = "";
      $("session-cwd").value = "";
      showNotice("");
      $("command-input").focus();
    } catch (error) {
      showNotice(`创建会话失败：${error.message} 请刷新会话列表检查结果。`, "error");
      // Do not automatically retry a mutation with an uncertain outcome.
      try { await loadSessions(); } catch (_) { /* Polling will recover. */ }
    } finally { state.creating = false; updateControls(); schedulePoll(); }
  }

  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = $("login-button");
    if (button.disabled) return;
    button.disabled = true;
    button.textContent = "连接中…";
    try {
      await api("/api/login", { method: "POST", body: { token: $("auth-token").value } });
      $("auth-token").value = "";
      setAuthenticated(true);
      showNotice("");
      await loadSessions({ autoCreate: true });
      schedulePoll();
    } catch (error) { showNotice(error.message, "error"); }
    finally { button.disabled = false; button.textContent = "连接"; schedulePoll(); }
  });

  $("new-session").addEventListener("click", () => {
    $("new-session-form").hidden = false;
    $("session-name").focus();
  });
  $("cancel-new-session").addEventListener("click", () => { $("new-session-form").hidden = true; });
  $("new-session-form").addEventListener("submit", (event) => {
    event.preventDefault();
    const body = {};
    if ($("session-name").value.trim()) body.name = $("session-name").value.trim();
    if ($("session-cwd").value.trim()) body.cwd = $("session-cwd").value.trim();
    createSession(body);
  });

  $("session-select").addEventListener("change", async (event) => {
    state.activeID = event.target.value;
    const session = state.sessions.find((s) => s.id === state.activeID);
    if (session) applySession(session);
    showNotice("");
    clearTimeout(state.pollTimer);
    await poll();
  });

  $("refresh-session").addEventListener("click", async () => {
    const button = $("refresh-session");
    if (button.disabled) return;
    button.disabled = true;
    try { await loadSessions(); showNotice(uncertain[state.activeID] ? "状态已刷新。请检查命令记录，提交锁定仍保留。" : "状态已刷新。"); }
    catch (error) { showNotice(error.message, "error"); }
    finally { button.disabled = false; schedulePoll(); }
  });

  $("close-session").addEventListener("click", async () => {
    const id = state.activeID;
    if (!id || $("close-session").disabled) return;
    state.pending = true;
    updateControls();
    try {
      await api(`/api/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
      await loadSessions();
      showNotice("会话已关闭。");
    } catch (error) { showNotice(`关闭会话失败：${error.message}`, "error"); }
    finally { state.pending = false; updateControls(); schedulePoll(); }
  });

  $("command-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if ($("run-command").disabled) return;
    const command = $("command-input").value;
    if (!command.trim()) { $("command-input").focus(); return; }
    const id = state.activeID;
    const timeout = Number($("timeout-seconds").value);
    if (!Number.isInteger(timeout) || timeout < 0 || timeout > 86400) { showNotice("超时须为 0 至 86400 的整数。", "error"); return; }
    uncertain[id] = { command, knownIDs: (state.session?.commands || []).map((c) => c.id), submittedAt: new Date().toISOString() };
    persistUncertain();
    state.pending = true;
    updateControls();
    showNotice("");
    try {
      const accepted = await api(`/api/sessions/${encodeURIComponent(id)}/commands`, { method: "POST", body: { command, timeout_seconds: timeout } });
      if (!accepted?.id) throw new APIError("服务器返回的命令提交结果无效。");
      delete uncertain[id];
      persistUncertain();
      if (state.activeID === id) {
        const previous = state.session?.commands || [];
        const commands = previous.some((c) => c.id === accepted.id) ? previous.map((c) => c.id === accepted.id ? accepted : c) : [...previous, accepted];
        applySession({ ...state.session, id, state: accepted.status === "running" ? "running" : "idle", active_command_id: accepted.status === "running" ? accepted.id : "", commands });
      }
    } catch (error) {
      if (error.status >= 400 && error.status < 500) {
        delete uncertain[id];
        persistUncertain();
        showNotice(`命令未提交：${error.message}`, "error");
      } else {
        showNotice("未收到命令提交结果。正在核对服务器记录，期间已锁定提交，防止重复执行。", "warning");
      }
    } finally {
      state.pending = false;
      updateControls();
      updateStateJSON();
      schedulePoll(0);
    }
  });

  $("command-input").addEventListener("keydown", (event) => {
    if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      if (!$("run-command").disabled) $("command-form").requestSubmit();
    }
  });

  $("resolve-submission").addEventListener("click", () => {
    if ($("resolve-submission").disabled) return;
    delete uncertain[state.activeID];
    persistUncertain();
    updateControls();
    updateStateJSON();
    showNotice("已按你的确认解除提交锁定。再次执行前，请核对命令记录。");
  });

  $("interrupt-command").addEventListener("click", async () => {
    if ($("interrupt-command").disabled) return;
    const id = state.activeID;
    state.pending = true;
    updateControls();
    try { await api(`/api/sessions/${encodeURIComponent(id)}/interrupt`, { method: "POST", body: {} }); showNotice("已发送中断信号。"); }
    catch (error) { showNotice(`中断失败：${error.message}`, "error"); }
    finally { state.pending = false; updateControls(); schedulePoll(0); }
  });

  async function sendInput(eof) {
    if (state.inputPending || state.session?.state !== "running") return;
    const id = state.activeID;
    const original = $("stdin-input").value;
    const data = eof ? "" : original + ($("stdin-newline").checked ? "\n" : "");
    state.inputPending = true;
    updateControls();
    try {
      await api(`/api/sessions/${encodeURIComponent(id)}/input`, { method: "POST", body: { data, eof } });
      if (!eof && $("stdin-input").value === original) $("stdin-input").value = "";
      showNotice(eof ? "已发送 EOF。" : "已发送标准输入。");
    } catch (error) { showNotice(`输入发送失败：${error.message} 请检查输出后再决定是否重发。`, "error"); }
    finally { state.inputPending = false; updateControls(); schedulePoll(0); }
  }
  $("stdin-form").addEventListener("submit", (event) => { event.preventDefault(); sendInput(false); });
  $("send-eof").addEventListener("click", () => sendInput(true));

  async function initialize() {
    try {
      state.info = await api("/api/info");
      $("server-info").textContent = `${state.info.shell || "Shell"} · ${state.info.platform || "Go"}`;
      $("version").textContent = state.info.version || "v1";
      setAuthenticated(!!state.info.authenticated);
      if (state.authenticated) { await loadSessions({ autoCreate: true }); schedulePoll(); }
      else $("auth-token").focus();
    } catch (error) {
      showNotice(error.message, "error");
      setTimeout(initialize, 2500);
    }
  }
  initialize();
})();
