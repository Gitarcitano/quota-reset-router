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
  main { max-width: 1100px; margin: 0 auto; padding: 24px; }
  h1 { font-size: 1.4rem; margin: 0 0 8px; }
  p { color: #aaa; line-height: 1.45; }
  label { display: grid; gap: 4px; font-size: 0.85rem; color: #bbb; }
  input, select, button { font: inherit; }
  input, select { background: #1c1c1c; color: #eee; border: 1px solid #333; border-radius: 8px; padding: 8px; }
  button { background: #f5f5f5; color: #111; border: 0; border-radius: 8px; padding: 8px 12px; cursor: pointer; }
  .row { display: flex; flex-wrap: wrap; gap: 12px; align-items: end; margin: 16px 0; }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 8px; border-bottom: 1px solid #2a2a2a; vertical-align: top; }
  .bad { color: #ffb4a8; }
  .ok { color: #b7ebc6; }
  #message { min-height: 1.2em; }
</style>
</head>
<body>
<main>
  <h1>Cotas</h1>
  <p>Uma sessao fica na mesma conta enquanto ela respeita a politica. Sessao nova escolhe, na maior prioridade ainda elegivel, a conta dentro da janela de liberacao e, depois, a que reinicia o limite de 7 dias primeiro. O percentual vem da ultima leitura, entao uma requisicao pode passar um pouco do limite.</p>
  <div class="row">
    <label>Chave de gerenciamento<input id="key" type="password" autocomplete="off"></label>
    <button id="load" type="button">Carregar</button>
  </div>
  <div class="row">
    <label>Modo<select id="mode"><option value="shadow">shadow</option><option value="active">active</option></select></label>
    <label>Teto de 5 horas<input id="five" type="number" min="0" max="100" step="1"></label>
    <label>Teto de 7 dias<input id="weekly" type="number" min="0" max="100" step="1"></label>
    <label>Liberar semanal antes de<input id="release" placeholder="5h"></label>
    <label>Afinidade<input id="ttl" placeholder="1h"></label>
  </div>
  <p id="message"></p>
  <table>
    <thead><tr><th>Conta</th><th>5 horas</th><th>7 dias</th><th>Estado</th><th>Teto 5h</th><th>Teto 7d</th><th>Liberar antes</th></tr></thead>
    <tbody id="accounts"></tbody>
  </table>
  <div class="row"><button id="save" type="button">Salvar limites</button></div>
</main>
<script>
const keyEl = document.querySelector("#key");
const message = document.querySelector("#message");
keyEl.value = sessionStorage.getItem("quota-reset-router-key") || "";
const headers = () => ({ "Authorization": "Bearer " + keyEl.value.trim(), "Content-Type": "application/json" });
const pct = (window) => window && window.utilization != null ? window.utilization.toFixed(1) + "%" : "sem leitura";
const when = (window) => window && window.resets_at ? new Date(window.resets_at).toLocaleString() : "sem reset";
function stateText(account) {
  if (account.stale) return "leitura desatualizada";
  if (account.block_reason) return account.block_reason;
  if (account.in_release_window) return "na janela de liberacao";
  return "disponivel";
}
async function load() {
  sessionStorage.setItem("quota-reset-router-key", keyEl.value.trim());
  message.textContent = "Carregando";
  const response = await fetch("/v0/management/plugins/quota-reset-router/status", { headers: headers() });
  if (!response.ok) { message.textContent = "Nao foi possivel ler o status (" + response.status + ")"; return; }
  const status = await response.json();
  document.querySelector("#mode").value = status.mode;
  document.querySelector("#five").value = status.five_hour_cap;
  document.querySelector("#weekly").value = status.weekly_cap;
  document.querySelector("#release").value = status.release_before;
  document.querySelector("#ttl").value = status.session_ttl;
  const body = document.querySelector("#accounts");
  body.replaceChildren();
  for (const [id, account] of Object.entries(status.accounts || {}).sort((a, b) => a[0].localeCompare(b[0]))) {
    const row = document.createElement("tr");
    row.dataset.id = id;
    const blocked = account.block_reason || account.stale;
    row.innerHTML = "<td></td><td></td><td></td><td></td><td><input type='number' min='0' max='100' step='1'></td><td><input type='number' min='0' max='100' step='1'></td><td><input></td>";
    row.children[0].textContent = (account.label || id) + "\n" + id;
    row.children[1].textContent = pct(account.quota && account.quota.five_hour) + "\n" + when(account.quota && account.quota.five_hour);
    row.children[2].textContent = pct(account.quota && account.quota.seven_day) + "\n" + when(account.quota && account.quota.seven_day);
    row.children[3].textContent = stateText(account);
    row.children[3].className = blocked ? "bad" : "ok";
    row.children[4].firstChild.value = account.five_hour_cap;
    row.children[5].firstChild.value = account.weekly_cap;
    row.children[6].firstChild.value = account.release_before;
    body.append(row);
  }
  message.textContent = status.accounts && Object.keys(status.accounts).length ? "" : "Nenhuma conta Claude ou Codex pronta para leitura.";
}
async function save() {
  const accounts = {};
  for (const row of document.querySelectorAll("#accounts tr")) {
    const inputs = row.querySelectorAll("input");
    accounts[row.dataset.id] = {
      five_hour_cap: Number(inputs[0].value),
      weekly_cap: Number(inputs[1].value),
      release_before: inputs[2].value.trim()
    };
  }
  const body = {
    mode: document.querySelector("#mode").value,
    five_hour_cap: Number(document.querySelector("#five").value),
    weekly_cap: Number(document.querySelector("#weekly").value),
    release_before: document.querySelector("#release").value.trim(),
    session_ttl: document.querySelector("#ttl").value.trim(),
    accounts
  };
  message.textContent = "Salvando";
  const response = await fetch("/v0/management/plugins/quota-reset-router/config", { method: "PATCH", headers: headers(), body: JSON.stringify(body) });
  message.textContent = response.ok ? "Limites salvos. O proxy recarrega a configuracao." : "Nao foi possivel salvar (" + response.status + ")";
  if (response.ok) setTimeout(load, 600);
}
document.querySelector("#load").addEventListener("click", () => load().catch((error) => { message.textContent = String(error); }));
document.querySelector("#save").addEventListener("click", () => save().catch((error) => { message.textContent = String(error); }));
if (keyEl.value) load().catch((error) => { message.textContent = String(error); });
</script>
</body>
</html>
`
