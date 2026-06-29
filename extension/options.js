const api = typeof browser !== "undefined" ? browser : chrome;

async function load() {
  const { serverUrl, token } = await api.storage.local.get(["serverUrl", "token"]);
  document.getElementById("serverUrl").value = serverUrl || "";
  document.getElementById("token").value = token || "";
}

document.getElementById("save").addEventListener("click", async () => {
  await api.storage.local.set({
    serverUrl: document.getElementById("serverUrl").value.trim(),
    token: document.getElementById("token").value.trim(),
  });
  const saved = document.getElementById("saved");
  saved.hidden = false;
  setTimeout(() => (saved.hidden = true), 1500);
});

load();
