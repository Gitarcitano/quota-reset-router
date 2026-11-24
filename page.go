package main

const panelHTML = `<!DOCTYPE html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Cotas</title>
<style>
  :root { color-scheme: dark; font-family: ui-sans-serif, system-ui, sans-serif; }
  body { margin: 0; background: #111; color: #eee; }
  main { max-width: 1200px; margin: 0 auto; padding: 24px; }
  h1 { font-size: 1.4rem; margin: 0 0 8px; }
  p { color: #aaa; line-height: 1.45; }
  label { display: grid; gap: 4px; font-size: 0.85rem; color: #bbb; }
  input, select, button { font: inherit; }
  input, select { background: #1c1c1c; color: #eee; border: 1px solid #333; border-radius: 8px; padding: 8px; }
  td input { width: 5em; }
  button { background: #f5f5f5; color: #111; border: 0; border-radius: 8px; padding: 8px 12px; cursor: pointer; }
  .row { display: flex; flex-wrap: wrap; gap: 12px; align-items: end; margin: 16px 0; }
  .scroll { overflow-x: auto; }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 8px; border-bottom: 1px solid #2a2a2a; vertical-align: top; white-space: pre-line; }
  .bad { color: #ffb4a8; }
  .ok { color: #b7ebc6; }
  #message { min-height: 1.2em; }
</style>
</head>
<body>
<main>
  <h1>Cotas</h1>
  <p>O plugin nao escolhe conta. No modo active ele reescreve a prioridade de cada conta Claude para que a que renova o limite de 7 dias primeiro fique no topo, e tira das novas escolhas a conta que chegou na reserva enquanto houver outra disponivel. A afinidade de sessao, as tentativas e o cooldown continuam com o proxy. No modo shadow o plano aparece aqui, mas nada e alterado.</p>
  <div class="row">
    <label>Chave de gerenciamento<input id="key" type="password" autocomplete="off"></label>
    <button id="load" type="button">Carregar</button>
  </div>
  <div class="row">
    <label>Modo<select id="mode"><option value="shadow">shadow</option><option value="active">active</option></select></label>
    <label>Intervalo de leitura<input id="poll" placeholder="30m"></label>
  </div>
  <p id="message"></p>
  <div class="scroll">
  <table>
    <thead><tr><th>Conta</th><th>5 horas</th><th>7 dias</th><th>Fonte / idade</th><th>Reserva 5h (%)</th><th>Reserva 7d (%)</th><th>Na reserva</th><th>Prioridade atual / planejada</th><th>Ultima escrita</th><th>Backoff</th></tr></thead>
    <tbody id="accounts"></tbody>
  </table>
  </div>
  <div class="row"><button id="save" type="button">Salvar</button></div>
</main>
<script>
const keyEl = document.querySelector("#key");
const message = document.querySelector("#message");
let configured = {};
let accountsFile = "";
try { keyEl.value = sessionStorage.getItem("quota-reset-router-key") || ""; } catch (error) {}
const headers = () => ({ "Authorization": "Bearer " + keyEl.value.trim(), "Content-Type": "application/json" });
const pct = (w) => w && w.utilization != null ? w.utilization.toFixed(1) + "%" : "sem leitura";
const when = (w) => w && w.resets_at ? new Date(w.resets_at).toLocaleString() : "sem reset";
const windowText = (w) => pct(w) + (w && w.status ? " (" + w.status + ")" : "") + "\n" + when(w);
function age(seconds) {
  if (seconds == null) return "sem leitura";
  if (seconds < 120) return seconds + "s";
  if (seconds < 7200) return Math.round(seconds / 60) + "min";
  return Math.round(seconds / 3600) + "h";
}
// The config key for a row: an existing ID or email key, otherwise the email, otherwise the ID.
// Keys compare like the backend does: trimmed and case-insensitive, the ID first.
function configKey(id, account) {
  const norm = (value) => (value || "").trim().toLowerCase();
  const keys = Object.keys(configured);
  const byId = keys.find((key) => norm(key) === norm(id));
  if (byId) return byId;
  const email = norm(account.email);
  const byEmail = email && keys.find((key) => norm(key) === email);
  return byEmail || account.email || id;
}
async function load() {
  try { sessionStorage.setItem("quota-reset-router-key", keyEl.value.trim()); } catch (error) {}
  message.textContent = "Carregando";
  const response = await fetch("/v0/management/plugins/quota-reset-router/status", { headers: headers() });
  if (!response.ok) { message.textContent = "Nao foi possivel ler o status (" + response.status + ")"; return; }
  const status = await response.json();
  configured = status.configured_accounts || {};
  accountsFile = status.accounts_file || "";
  document.querySelector("#mode").value = status.mode;
  document.querySelector("#poll").value = status.poll_interval;
  const body = document.querySelector("#accounts");
  body.replaceChildren();
  const rows = Object.entries(status.accounts || {}).sort((a, b) => b[1].planned_priority - a[1].planned_priority || a[0].localeCompare(b[0]));
  for (const [id, account] of rows) {
    const row = document.createElement("tr");
    row.dataset.key = configKey(id, account);
    row.innerHTML = "<td></td><td></td><td></td><td></td><td><input type='number' min='0' max='95' step='1'></td><td><input type='number' min='0' max='95' step='1'></td><td></td><td></td><td></td><td></td>";
    row.children[0].textContent = (account.email || account.label || id) + "\n" + id;
    row.children[1].textContent = windowText(account.five_hour);
    row.children[2].textContent = windowText(account.seven_day);
    row.children[3].textContent = (account.source || "-") + "\n" + age(account.age_seconds);
    row.children[4].firstChild.value = account.five_hour_reserve ?? "";
    row.children[5].firstChild.value = account.weekly_reserve ?? "";
    row.children[6].textContent = account.at_reserve ? "sim" : (account.exhausted ? "esgotada" : (account.weekly_reserve_released ? "liberada ate o reset" : "nao"));
    if (!account.exhausted && account.weekly_release_at) {
      row.children[6].textContent += "\nlibera " + new Date(account.weekly_release_at).toLocaleString("pt-BR", { weekday: "short", hour: "2-digit", minute: "2-digit" });
    }
    row.children[6].className = account.at_reserve || account.exhausted ? "bad" : "ok";
    row.children[7].textContent = account.priority + " / " + account.planned_priority;
    row.children[8].textContent = account.last_write || "-";
    row.children[8].className = account.last_write && account.last_write !== "ok" ? "bad" : "";
    row.children[9].textContent = account.poll_backoff ? account.poll_backoff + "\n" + (account.poll_error || "") : "-";
    body.append(row);
  }
  let note = rows.length ? "" : "Nenhuma conta Claude elegivel.";
  if (rows.length && !status.weekly_reset_known) note = "Nenhum reset semanal conhecido ainda. As prioridades nao serao alteradas.";
  if (accountsFile) note = (note ? note + " " : "") + "Reservas no arquivo " + accountsFile + (status.accounts_file_error ? " (erro: " + status.accounts_file_error + ")" : "") + ".";
  if (status.worker_failed) note = "Erro interno: o plugin parou de excluir contas e de escrever prioridades.";
  message.textContent = note;
}
async function save() {
  const accounts = Object.assign({}, configured);
  for (const row of document.querySelectorAll("#accounts tr")) {
    const inputs = row.querySelectorAll("input");
    const entry = {};
    if (inputs[0].value !== "") entry.five_hour_reserve = Number(inputs[0].value);
    if (inputs[1].value !== "") entry.weekly_reserve = Number(inputs[1].value);
    if (Object.keys(entry).length) accounts[row.dataset.key] = entry;
    else delete accounts[row.dataset.key];
  }
  const body = { mode: document.querySelector("#mode").value, poll_interval: document.querySelector("#poll").value.trim() || "30m" };
  message.textContent = "Salvando";
  if (accountsFile) {
    // Reserves go to the shared file; mode and poll interval stay in this machine's config.
    // Sending what was loaded lets the plugin apply only the reserves edited here.
    const saved = await fetch("/v0/management/plugins/quota-reset-router/accounts", { method: "POST", headers: headers(), body: JSON.stringify({ accounts, previous: configured }) });
    if (!saved.ok) {
      let detail = String(saved.status);
      try { detail = (await saved.json()).error || detail; } catch (error) {}
      message.textContent = "Nao foi possivel salvar as reservas (" + detail + ")";
      // Mode and poll interval live in this machine's own config and can still be saved.
      await fetch("/v0/management/plugins/quota-reset-router/config", { method: "PATCH", headers: headers(), body: JSON.stringify(body) });
      return;
    }
  } else {
    body.accounts = accounts;
  }
  const response = await fetch("/v0/management/plugins/quota-reset-router/config", { method: "PATCH", headers: headers(), body: JSON.stringify(body) });
  if (response.ok) message.textContent = "Configuracao salva. O proxy recarrega o plugin.";
  else if (accountsFile) message.textContent = "Reservas salvas no arquivo compartilhado, mas modo e intervalo nao (" + response.status + ")";
  else message.textContent = "Nao foi possivel salvar (" + response.status + ")";
  if (response.ok) setTimeout(load, 600);
}
document.querySelector("#load").addEventListener("click", () => load().catch((error) => { message.textContent = String(error); }));
document.querySelector("#save").addEventListener("click", () => save().catch((error) => { message.textContent = String(error); }));
if (keyEl.value) load().catch((error) => { message.textContent = String(error); });
</script>
</body>
</html>
`
