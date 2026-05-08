// YTDisc v2 — frontend logic.
//
// Backend bindings: window.go.main.App.<Method>(...). All async.
// Wails event API on window.runtime.

const App = window.go.main.App;
const { OpenFileDialog, EventsOn } = window.runtime;

// ---- DOM refs ------------------------------------------------------------

const $ = (id) => document.getElementById(id);

// ---- Global state --------------------------------------------------------

const state = {
  bootState: null,             // BootState DTO from Go
  currentAccount: null,        // Account or null
  accounts: [],                // full account list
  tab: "videos",               // active tab name
  // Videos tab
  selectedChannel: null,
  currentFolder: null,
  selectedItem: null,
  itemsByKey: new Map(),
  manageSubsActive: false,
  // Music tab
  selectedArtist: null,
  selectedAlbum: null,
  artistList: [],              // ArtistInfo[]
  expandedArtists: new Set(),  // artist names with their album list expanded
  // Player (videos)
  playerCurrent: null,
  playerSaveTimer: null,
  playerResuming: false,
  // Music player
  music: {
    queue: [],                 // SongInfo[]
    queueIndex: 0,
    paused: true,
    audio: null,               // single shared <audio>
    popoutOpen: false,
    shuffle: false,
  },
  // Misc
  editCap: null,
  preparingPoll: null,
  renameTarget: null,
  confirmAction: null,
  confirmAltAction: null,
};

const RESUME_HEAD_SECS = 45;
const RESUME_TAIL_SECS = 20;
const POSITION_SAVE_INTERVAL_MS = 5000;

// ---- Init ----------------------------------------------------------------

async function init() {
  state.bootState = await App.GetBootState();
  if (state.bootState.state === "no-library") {
    renderNoLibrary(state.bootState.message);
    return;
  }
  if (state.bootState.state === "data-corrupt") {
    // Don't auto-recover — the user might want to restore from a
    // backup, and silently rewriting accounts.json with an empty
    // file would destroy any recoverable account data.
    renderNoLibrary(
      `${state.bootState.message || "Drive data is corrupt."}\n` +
      `Restore .data/accounts.json from a backup, or delete it to start fresh.`
    );
    return;
  }
  if (state.bootState.state === "needs-upgrade") {
    showModal($("upgrade-modal"));
    return;
  }
  if (state.bootState.state === "needs-first-account") {
    setupProfileForm("first-account");
    showModal($("first-account-modal"));
    setTimeout(() => $("first-account-name").focus(), 50);
    return;
  }
  await postBoot();
}

// realAccount unwraps the value-type Account that the backend sends
// (BootState.currentAccount and GetCurrentAccount() are both value
// types — when there's no current account the field is the
// zero-value Account with id == ""). Use this when assigning to
// state.currentAccount so the rest of the JS can keep doing its
// existing `if (state.currentAccount)` and `state.currentAccount?.X`
// checks.
function realAccount(maybeAcct) {
  return maybeAcct && maybeAcct.id ? maybeAcct : null;
}

// postBoot is everything that runs once we're definitively in the
// "ready" state (i.e. there's a current account). Called after init,
// after AcceptDriveUpgrade, and after first-account creation. Wraps
// the body in a try/catch so a transient render error doesn't leave
// the user staring at a blank webview after CreateAccount succeeded
// backend-side.
let postBootRan = false;
async function postBoot() {
  try {
    state.bootState = await App.GetBootState();
    state.currentAccount = realAccount(state.bootState.currentAccount);
    state.accounts = await App.GetAccounts();
    await refreshEditCapability(false);
    if (!postBootRan) {
      EventsOn("ytdlp-progress", onYtdlpProgress);
      window.addEventListener("focus", () => refreshEditCapability(true));
      window.addEventListener("beforeunload", () => savePlayerPosition({ flushOnly: true }));
      window.addEventListener("pagehide", () => savePlayerPosition({ flushOnly: true }));
      postBootRan = true;
    }
    setActiveTab(state.currentAccount?.lastTab || "videos");
    document.body.classList.toggle("is-editor", !!state.currentAccount?.isEditor);
    renderUserBar();
    switchTab(state.tab);
  } catch (err) {
    console.error("postBoot failed:", err);
    alert(`Couldn't finish startup: ${err}\n\nThe account was created, but the app needs to restart.`);
  }
}

function renderNoLibrary(msg) {
  $("app").innerHTML = `<div class="message"><div>
    <p>${escapeHTML(msg || "No drive found.")}</p>
    <p>Place this app on a USB stick next to a folder named
    <code>Videos/</code> with one subfolder per channel, plus an
    optional <code>Music/</code> folder for albums.</p>
  </div></div>`;
}

// ---- Boot modals ---------------------------------------------------------

$("upgrade-accept").addEventListener("click", async () => {
  $("upgrade-accept").disabled = true;
  $("upgrade-quit").disabled = true;
  try {
    await App.AcceptDriveUpgrade();
    hideModal($("upgrade-modal"));
    setupProfileForm("first-account");
    showModal($("first-account-modal"));
    setTimeout(() => $("first-account-name").focus(), 50);
  } catch (err) {
    alert(`Upgrade failed: ${String(err)}`);
    $("upgrade-accept").disabled = false;
    $("upgrade-quit").disabled = false;
  }
});

$("upgrade-quit").addEventListener("click", () => App.QuitApp());

// First-account creation
$("first-account-save").addEventListener("click", async () => {
  await tryCreateAccount("first-account");
});
$("first-account-name").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); $("first-account-save").click(); }
});

async function tryCreateAccount(prefix) {
  const name = $(prefix + "-name").value.trim();
  const a = $(prefix + "-colorA").value;
  const b = $(prefix + "-colorB").value;
  const angle = parseInt($(prefix + "-angle").value, 10) || 0;
  const status = $(prefix + "-status");
  if (!name) {
    setStatusEl(status, "Username can't be empty.", "error");
    return;
  }
  setStatusEl(status, "Creating…", "info");
  try {
    await App.CreateAccount(name, a, b, angle);
    if (prefix === "first-account") {
      hideModal($("first-account-modal"));
      await postBoot();
    } else {
      hideModal($("add-account-modal"));
      state.accounts = await App.GetAccounts();
      if (state.tab === "accounts") renderAccountsTab();
    }
  } catch (err) {
    setStatusEl(status, String(err), "error");
  }
}

// ---- Profile form (live conic-gradient preview) --------------------------

function setupProfileForm(prefix) {
  const a = $(prefix + "-colorA");
  const b = $(prefix + "-colorB");
  const ang = $(prefix + "-angle");
  const out = $(prefix + "-angle-out");
  const preview = $(prefix + "-preview");
  const update = () => {
    const av = a.value, bv = b.value, angle = ang.value;
    out.textContent = `${angle}°`;
    setProfileGradient(preview, av, bv, parseInt(angle, 10));
  };
  a.oninput = b.oninput = ang.oninput = update;
  update();
}

function setProfileGradient(el, colorA, colorB, angle) {
  // The conic gradient repeats the start color at the end so the
  // 0°↔360° seam is hidden. Looks like a smooth ring.
  el.style.background =
    `conic-gradient(from ${angle}deg, ${colorA}, ${colorB}, ${colorA})`;
}

// ---- Tabs ----------------------------------------------------------------

document.querySelectorAll(".tab[data-tab]").forEach((tab) => {
  tab.addEventListener("click", () => switchTab(tab.dataset.tab));
});

function setActiveTab(name) {
  state.tab = name;
  document.body.dataset.tab = name;
  for (const t of document.querySelectorAll(".tab[data-tab]")) {
    t.classList.toggle("active", t.dataset.tab === name);
  }
  for (const c of document.querySelectorAll("[data-tab-content]")) {
    c.classList.toggle("hidden", c.dataset.tabContent !== name);
  }
}

async function switchTab(name) {
  setActiveTab(name);
  if (state.currentAccount && !state.currentAccount.isEditor) {
    App.UpdateLastTab(name).catch(() => {});
  }
  if (name === "accounts") {
    await renderAccountsTab();
  } else if (name === "videos") {
    await renderVideosTab();
  } else if (name === "music") {
    await renderMusicTab();
  }
}

// ---- "Logged in as ..." strip --------------------------------------------

function renderUserBar() {
  const acct = state.currentAccount;
  if (!acct) {
    $("user-bar-name").textContent = "—";
    $("user-bar-avatar").style.background = "transparent";
    return;
  }
  $("user-bar-name").textContent = acct.username;
  setProfileGradient($("user-bar-avatar"), acct.colorA, acct.colorB, acct.angle);
}

// ===========================================================================
// ACCOUNTS TAB
// ===========================================================================

async function renderAccountsTab() {
  state.accounts = await App.GetAccounts();
  const list = $("accounts-list");
  list.innerHTML = "";
  for (const acct of state.accounts) {
    const li = document.createElement("li");
    li.className = "account-row";
    if (state.currentAccount && acct.id === state.currentAccount.id) {
      li.classList.add("selected");
    }
    const isCurrent = state.currentAccount && acct.id === state.currentAccount.id;
    li.innerHTML = `
      <span class="profile-pic profile-pic-md"></span>
      <div class="row-main">
        <span class="title">${escapeHTML(acct.username)}${isCurrent ? ` <span class="current-badge">you</span>` : ""}</span>
        <span class="meta">${acct.isEditor ? "Editor — can modify the library" : "Account"}</span>
      </div>
      <div class="row-actions">
        ${acct.isEditor ? "" :
          `<button class="row-btn" data-action="del" title="Delete account">${icon('trash')}</button>`}
      </div>`;
    setProfileGradient(li.querySelector(".profile-pic"), acct.colorA, acct.colorB, acct.angle);
    li.addEventListener("click", async (e) => {
      const action = e.target.closest("[data-action]")?.dataset?.action;
      if (action === "del") {
        e.stopPropagation();
        if (!confirm(`Delete account "${acct.username}"? Their watch progress and playlists will be permanently removed.`)) return;
        try {
          await App.DeleteAccount(acct.id);
          // After deletion, re-read boot state. If the backend
          // dropped us into "needs-first-account" (meaning no real
          // accounts remain), show the modal — same one as fresh-
          // drive setup, blocking so the user can't escape into a
          // logged-out app even by quitting and reopening.
          const boot = await App.GetBootState();
          if (boot.state === "needs-first-account") {
            state.bootState = boot;
            state.currentAccount = null;
            renderUserBar();
            setupProfileForm("first-account");
            showModal($("first-account-modal"));
            setTimeout(() => $("first-account-name").focus(), 50);
            return;
          }
          state.currentAccount = realAccount(await App.GetCurrentAccount());
          document.body.classList.toggle("is-editor", !!state.currentAccount?.isEditor);
          renderUserBar();
          state.accounts = await App.GetAccounts();
          renderAccountsTab();
        } catch (err) {
          alert(String(err));
        }
        return;
      }
      // Click on a row → switch to that account.
      try {
        await App.SwitchAccount(acct.id);
        state.currentAccount = realAccount(await App.GetCurrentAccount());
        document.body.classList.toggle("is-editor", !!state.currentAccount?.isEditor);
        renderUserBar();
        await refreshEditCapability(false);
        // For Editor we deliberately STAY on the Accounts tab — Editor
        // is for admin tasks and the user picks where to go next. For
        // a regular account, jump to whatever tab they had open at
        // last close so they land where they left off.
        if (!state.currentAccount?.isEditor) {
          switchTab(state.currentAccount?.lastTab || "videos");
        } else {
          // Just re-render the Accounts tab so the "selected" highlight
          // moves to the Editor row and the stats refresh.
          renderAccountsTab();
        }
      } catch (err) {
        alert(String(err));
      }
    });
    list.appendChild(li);
  }
  // Stats panel
  await renderStatsPanel();
}

async function renderStatsPanel() {
  const status = await App.Status();
  const panel = $("stats-panel");
  // The library scan runs on a background goroutine after startup,
  // so the very first Status() call (right after boot) returns
  // ok=false even on a healthy drive. Show a placeholder and re-poll
  // until the scan finishes — without this, the stats panel showed
  // "Couldn't find a Videos/ folder" until the user clicked
  // somewhere, which made it look like the app was permanently
  // confused.
  if (!status.ok) {
    panel.innerHTML = `<p class="muted">${escapeHTML(status.message || "Loading library…")}</p>`;
    clearTimeout(state.statsRetryTimer);
    state.statsRetryTimer = setTimeout(() => {
      // Only retry if the user is still on the Accounts tab; if they
      // switched away there's no point updating an off-screen panel,
      // and the next Accounts visit will trigger a fresh render.
      if (state.tab === "accounts") renderStatsPanel();
    }, 800);
    return;
  }
  clearTimeout(state.statsRetryTimer);
  state.statsRetryTimer = null;
  panel.innerHTML = `
    <h2 class="stats-heading">Library</h2>
    <div class="stats-grid">
      <div class="stat-cell"><div class="stat-num">${status.channels}</div><div class="stat-label">channels</div></div>
      <div class="stat-cell"><div class="stat-num">${status.videos}</div><div class="stat-label">videos</div></div>
      <div class="stat-cell"><div class="stat-num">${formatDuration(status.totalSecs)}</div><div class="stat-label">video runtime</div></div>
      <div class="stat-cell"><div class="stat-num">${formatBytes(status.totalBytes)}</div><div class="stat-label">video size</div></div>
      <div class="stat-cell"><div class="stat-num">${status.artists}</div><div class="stat-label">artists</div></div>
      <div class="stat-cell"><div class="stat-num">${status.albums}</div><div class="stat-label">albums</div></div>
      <div class="stat-cell"><div class="stat-num">${status.songs}</div><div class="stat-label">songs</div></div>
      <div class="stat-cell"><div class="stat-num">${formatDuration(status.musicSecs)}</div><div class="stat-label">music runtime</div></div>
      <div class="stat-cell"><div class="stat-num">${formatBytes(status.musicBytes)}</div><div class="stat-label">music size</div></div>
    </div>
    <p class="muted small-print">Drive: <code>${escapeHTML(status.driveRoot)}</code></p>`;
}

$("add-account-btn").addEventListener("click", () => {
  setupProfileForm("add-account");
  $("add-account-name").value = "";
  setStatusEl($("add-account-status"), "");
  showModal($("add-account-modal"));
  setTimeout(() => $("add-account-name").focus(), 50);
});
$("add-account-save").addEventListener("click", () => tryCreateAccount("add-account"));
$("add-account-cancel").addEventListener("click", () => hideModal($("add-account-modal")));
$("add-account-close").addEventListener("click", () => hideModal($("add-account-modal")));
$("add-account-name").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); $("add-account-save").click(); }
});

// ===========================================================================
// VIDEOS TAB
// ===========================================================================

async function renderVideosTab() {
  state.manageSubsActive = false;
  $("subs-view").classList.add("hidden");
  const channels = await App.Channels();
  renderChannels(channels);
  if (channels.length > 0 && !state.selectedChannel) {
    selectChannel(channels[0].name);
  } else if (state.selectedChannel) {
    selectChannel(state.selectedChannel);
  } else {
    $("video-list").innerHTML = "";
    $("videos-header").textContent = "Videos";
    $("detail").className = "detail-empty";
    $("detail").innerHTML = "<p>Select a video</p>";
  }
}

function renderChannels(channels) {
  const list = $("channel-list");
  list.innerHTML = "";
  for (const c of channels) {
    const li = document.createElement("li");
    li.dataset.channel = c.name;
    li.innerHTML = `
      <div class="row-main">
        <span class="title">${escapeHTML(c.name)}</span>
        <span class="meta">${c.videoCount} video${c.videoCount === 1 ? "" : "s"} · ${formatDuration(c.totalSecs)}</span>
      </div>
      <div class="row-actions editor-only">
        <button class="row-btn" data-action="rename-channel" title="Rename">${icon('pencil-simple')}</button>
        <button class="row-btn" data-action="delete-channel" title="Delete">${icon('trash')}</button>
      </div>`;
    li.addEventListener("click", (e) => {
      const action = e.target.closest("[data-action]")?.dataset?.action;
      if (action === "rename-channel") { e.stopPropagation(); openRenameChannel(c.name); }
      else if (action === "delete-channel") { e.stopPropagation(); confirmDeleteChannel(c.name, c.videoCount); }
      else { selectChannel(c.name); }
    });
    list.appendChild(li);
  }
}

async function selectChannel(name) {
  state.selectedChannel = name;
  state.currentFolder = null;
  state.selectedItem = null;
  highlightSelection($("channel-list"), "channel", name);
  $("videos-header").textContent = name;
  $("detail").className = "detail-empty";
  $("detail").innerHTML = "<p>Select a video</p>";
  await loadAndRenderItems();
}

async function loadAndRenderItems() {
  const ch = state.selectedChannel;
  if (!ch) return;
  const key = state.currentFolder ? `${ch}\0${state.currentFolder}` : ch;
  let items = state.itemsByKey.get(key);
  if (!items) {
    items = await App.Items(ch, state.currentFolder || "");
    state.itemsByKey.set(key, items);
  }
  $("videos-header").textContent = state.currentFolder ? `${ch} / ${state.currentFolder}` : ch;
  renderItems(items);
}

function renderItems(items) {
  const videoList = $("video-list");
  videoList.innerHTML = "";
  if (state.currentFolder) {
    const back = document.createElement("li");
    back.className = "back-row";
    back.innerHTML = `<div class="row-main"><span class="title">← Back to ${escapeHTML(state.selectedChannel)}</span></div>`;
    back.addEventListener("click", () => {
      state.currentFolder = null;
      state.selectedItem = null;
      $("detail").className = "detail-empty";
      $("detail").innerHTML = "<p>Select a video</p>";
      loadAndRenderItems();
    });
    videoList.appendChild(back);
  }
  for (const it of items) {
    const li = document.createElement("li");
    if (it.kind === "folder") {
      li.dataset.folder = it.name;
      li.innerHTML = `
        <div class="row-main">
          <span class="title">${icon('folder')} ${escapeHTML(it.name)}</span>
          <span class="meta">${it.videoCount} video${it.videoCount === 1 ? "" : "s"} · ${formatDuration(it.totalSecs)}</span>
        </div>
        <div class="row-actions editor-only">
          <button class="row-btn" data-action="rename-folder" title="Rename">${icon('pencil-simple')}</button>
          <button class="row-btn" data-action="delete-folder" title="Empty / delete">${icon('trash')}</button>
        </div>`;
      li.addEventListener("click", (e) => {
        const a = e.target.closest("[data-action]")?.dataset?.action;
        if (a === "rename-folder") { e.stopPropagation(); openRenameFolder(it.name); }
        else if (a === "delete-folder") { e.stopPropagation(); confirmDeleteFolder(it.name, it.videoCount); }
        else { enterFolder(it.name); }
      });
    } else {
      li.dataset.relpath = it.relPath;
      li.innerHTML = `
        <div class="row-main">
          <span class="title">${escapeHTML(it.name)}</span>
          <span class="meta">${formatDuration(it.totalSecs)}${it.width ? ` · ${it.width}×${it.height}` : ""}</span>
        </div>
        <div class="row-actions editor-only">
          <button class="row-btn" data-action="move-video" title="Move">${icon('arrow-elbow-up-right')}</button>
          <button class="row-btn" data-action="rename-video" title="Rename">${icon('pencil-simple')}</button>
          <button class="row-btn" data-action="delete-video" title="Delete">${icon('trash')}</button>
        </div>`;
      li.addEventListener("click", (e) => {
        const a = e.target.closest("[data-action]")?.dataset?.action;
        if (a === "move-video") { e.stopPropagation(); openMoveVideo(it); }
        else if (a === "rename-video") { e.stopPropagation(); openRenameVideo(it); }
        else if (a === "delete-video") { e.stopPropagation(); confirmDeleteVideo(it); }
        else { selectVideo(it); }
      });
    }
    videoList.appendChild(li);
  }
}

function enterFolder(name) {
  state.currentFolder = name;
  state.selectedItem = null;
  $("detail").className = "detail-empty";
  $("detail").innerHTML = "<p>Select a video</p>";
  loadAndRenderItems();
}

async function selectVideo(v) {
  state.selectedItem = v;
  highlightSelection($("video-list"), "relpath", v.relPath);
  await renderDetail(v);
}

async function renderDetail(v) {
  const has = await App.HasThumbnail(v.relPath);
  const thumbURL = has ? `/thumb/${encodePath(v.relPath)}?t=${Date.now()}` : null;
  let resumeAt = 0;
  try { resumeAt = await App.GetPosition(v.relPath); } catch {}
  const willResume = shouldResumeAt(resumeAt, v.totalSecs);
  const detail = $("detail");
  detail.className = "detail";
  detail.innerHTML = `
    <div class="detail-thumb">
      ${thumbURL ? `<img src="${thumbURL}" alt="">` : `<div class="placeholder">No thumbnail</div>`}
      <div class="duration-tag">${formatDuration(v.totalSecs)}</div>
    </div>
    <h1 class="detail-title">${escapeHTML(v.name)}</h1>
    <div class="detail-channel">${escapeHTML(v.channel)}${v.folder ? ` · ${icon('folder')} ${escapeHTML(v.folder)}` : ""}</div>
    <div class="detail-stats">
      ${v.width ? `<span>${v.width} × ${v.height}</span>` : ""}
      <span>${formatBytes(v.sizeBytes)}</span>
    </div>
    <div class="detail-actions">
      <button class="play-btn" id="play-btn">${icon('play')} ${willResume ? `Resume at ${formatDuration(resumeAt)}` : "Play"}</button>
      ${willResume ? `<button class="secondary-btn" id="play-from-start-btn">From start</button>` : ""}
      <button class="secondary-btn editor-only" id="set-thumb-btn">${has ? "Change thumbnail…" : "Set thumbnail…"}</button>
    </div>`;
  $("play-btn").addEventListener("click", () => playVideo(v));
  $("play-from-start-btn")?.addEventListener("click", () => playVideo(v, { fromStart: true }));
  $("set-thumb-btn")?.addEventListener("click", () => openThumbModal(v));
}

// ---- Manage subscriptions ------------------------------------------------

$("manage-subs-btn").addEventListener("click", openSubsView);
$("subs-close").addEventListener("click", closeSubsView);

async function openSubsView() {
  state.manageSubsActive = true;
  $("subs-view").classList.remove("hidden");
  const all = await App.AllChannels();
  const list = $("subs-list");
  list.innerHTML = "";
  if (all.length === 0) {
    list.innerHTML = `<li class="empty-row">No channels exist yet${state.currentAccount?.isEditor ? " — add one with the + button when you exit." : "."}</li>`;
    return;
  }
  for (const c of all) {
    const li = document.createElement("li");
    li.className = "subs-row";
    li.innerHTML = `
      <label class="subs-check">
        <input type="checkbox" ${c.subscribed ? "checked" : ""} ${state.currentAccount?.isEditor ? "disabled" : ""} />
        <span class="title">${escapeHTML(c.name)}</span>
        <span class="meta">${c.videoCount} video${c.videoCount === 1 ? "" : "s"}</span>
      </label>`;
    const checkbox = li.querySelector("input");
    checkbox.addEventListener("change", async (e) => {
      try {
        if (e.target.checked) await App.Subscribe(c.name);
        else await App.Unsubscribe(c.name);
        // Live-update the channel list under the subs view.
        renderChannels(await App.Channels());
      } catch (err) {
        alert(String(err));
        e.target.checked = !e.target.checked;
      }
    });
    list.appendChild(li);
  }
}

function closeSubsView() {
  state.manageSubsActive = false;
  $("subs-view").classList.add("hidden");
}

// ===========================================================================
// VIDEO PLAYER (overlay)
// ===========================================================================

const player = $("player");
const playerVideo = $("player-video");
const playerTitle = $("player-title");

function shouldResumeAt(pos, durationSec) {
  if (!pos || pos <= 0) return false;
  if (pos < RESUME_HEAD_SECS) return false;
  if (durationSec && pos > durationSec - RESUME_TAIL_SECS) return false;
  return true;
}

async function playVideo(v, opts = {}) {
  state.playerCurrent = v;
  state.playerResuming = false;
  playerVideo.src = `/video/${encodePath(v.relPath)}`;
  playerTitle.textContent = `${v.channel} — ${v.name}`;
  player.classList.remove("hidden");
  let resumeAt = 0;
  if (!opts.fromStart) {
    try { resumeAt = await App.GetPosition(v.relPath); } catch {}
  }
  if (shouldResumeAt(resumeAt, v.totalSecs)) {
    state.playerResuming = true;
    const seek = () => {
      try { playerVideo.currentTime = resumeAt; } catch {}
      setTimeout(() => { state.playerResuming = false; }, 200);
      playerVideo.removeEventListener("loadedmetadata", seek);
    };
    playerVideo.addEventListener("loadedmetadata", seek);
  }
  startPositionSaver();
  playerVideo.play().catch(() => {});
}

function closePlayer() {
  savePlayerPosition();
  stopPositionSaver();
  if (document.fullscreenElement) document.exitFullscreen?.();
  player.classList.add("hidden");
  playerVideo.pause();
  playerVideo.removeAttribute("src");
  playerVideo.load();
  if (state.selectedItem && state.selectedItem.kind === "video") renderDetail(state.selectedItem);
  state.playerCurrent = null;
}

function toggleFullscreen() {
  if (document.fullscreenElement) { document.exitFullscreen?.(); return; }
  if (playerVideo.requestFullscreen) playerVideo.requestFullscreen().catch(() => {});
  else if (playerVideo.webkitRequestFullscreen) playerVideo.webkitRequestFullscreen();
}

$("player-close").addEventListener("click", closePlayer);
$("player-fullscreen").addEventListener("click", toggleFullscreen);
playerVideo.addEventListener("ended", () => {
  if (state.playerCurrent) App.ClearPosition(state.playerCurrent.relPath).catch(() => {});
});
playerVideo.addEventListener("pause", () => savePlayerPosition());

function startPositionSaver() {
  stopPositionSaver();
  state.playerSaveTimer = setInterval(savePlayerPosition, POSITION_SAVE_INTERVAL_MS);
}
function stopPositionSaver() {
  if (state.playerSaveTimer) { clearInterval(state.playerSaveTimer); state.playerSaveTimer = null; }
}
function savePlayerPosition({ flushOnly = false } = {}) {
  const v = state.playerCurrent;
  if (!v) return;
  if (state.playerResuming && !flushOnly) return;
  const pos = playerVideo.currentTime;
  if (!isFinite(pos) || pos <= 1) return;
  if (v.totalSecs && pos > v.totalSecs - RESUME_TAIL_SECS) {
    App.ClearPosition(v.relPath).catch(() => {});
    return;
  }
  App.SavePosition(v.relPath, pos).catch(() => {});
}

// ===========================================================================
// MUSIC TAB
// ===========================================================================

async function renderMusicTab() {
  state.artistList = await App.MusicArtists();
  renderArtistList();
  renderNowPlaying();
}

function renderArtistList() {
  const list = $("artist-list");
  list.innerHTML = "";
  if (state.artistList.length === 0) {
    list.innerHTML = `<li class="empty-row">No music yet${state.currentAccount?.isEditor ? " — add some with the + button" : ""}.</li>`;
    return;
  }
  for (const ar of state.artistList) {
    const li = document.createElement("li");
    li.className = "artist-row";
    const expanded = state.expandedArtists.has(ar.name);
    li.innerHTML = `
      <div class="row-main artist-head">
        <span class="title">${icon(expanded ? 'caret-down' : 'caret-right')} ${escapeHTML(ar.name)}</span>
        <span class="meta">${ar.albumCount} album${ar.albumCount === 1 ? "" : "s"} · ${ar.songCount} song${ar.songCount === 1 ? "" : "s"}</span>
      </div>
      <div class="row-actions editor-only">
        <button class="row-btn" data-action="rename-artist" title="Rename artist">${icon('pencil-simple')}</button>
        <button class="row-btn" data-action="del-artist" title="Delete artist">${icon('trash')}</button>
      </div>`;
    li.addEventListener("click", (e) => {
      const a = e.target.closest("[data-action]")?.dataset?.action;
      if (a === "rename-artist") { e.stopPropagation(); openRenameArtist(ar.name); return; }
      if (a === "del-artist") {
        e.stopPropagation();
        if (!confirm(`Move artist "${ar.name}" and all their albums + songs to trash?`)) return;
        App.DeleteArtist(ar.name).then(() => renderMusicTab()).catch((err) => alert(String(err)));
        return;
      }
      if (state.expandedArtists.has(ar.name)) state.expandedArtists.delete(ar.name);
      else state.expandedArtists.add(ar.name);
      renderArtistList();
    });
    list.appendChild(li);
    if (expanded) {
      for (const al of ar.albums) {
        const ali = document.createElement("li");
        ali.className = "album-row";
        if (state.selectedArtist === ar.name && state.selectedAlbum === al.name) ali.classList.add("selected");
        ali.innerHTML = `
          <div class="row-main">
            <span class="title">${icon('vinyl-record')} ${escapeHTML(al.name)}</span>
            <span class="meta">${al.songCount} song${al.songCount === 1 ? "" : "s"} · ${formatDuration(al.totalSecs)}</span>
          </div>
          <div class="row-actions editor-only">
            <button class="row-btn" data-action="rename-album" title="Rename album">${icon('pencil-simple')}</button>
            <button class="row-btn" data-action="del-album" title="Delete album">${icon('trash')}</button>
          </div>`;
        ali.addEventListener("click", (e) => {
          const a = e.target.closest("[data-action]")?.dataset?.action;
          if (a === "rename-album") { e.stopPropagation(); openRenameAlbum(ar.name, al.name); return; }
          if (a === "del-album") {
            e.stopPropagation();
            if (!confirm(`Move album "${al.name}" to trash?`)) return;
            App.DeleteAlbum(ar.name, al.name).then(() => renderMusicTab()).catch((err) => alert(String(err)));
            return;
          }
          selectAlbum(ar.name, al.name);
        });
        list.appendChild(ali);
      }
    }
  }
}

async function selectAlbum(artist, album) {
  state.selectedArtist = artist;
  state.selectedAlbum = album;
  renderArtistList();
  $("songs-header").textContent = `${artist} — ${album}`;
  const songs = await App.AlbumSongs(artist, album);
  const songList = $("song-list");
  songList.innerHTML = "";
  $("album-controls").classList.toggle("hidden", songs.length === 0);
  for (const s of songs) {
    const li = document.createElement("li");
    li.dataset.relpath = s.relPath;
    li.innerHTML = `
      <div class="row-main">
        <span class="title">${escapeHTML(s.title)}${s.hasMV ? ` <span class="mv-badge" title="Music video attached">${icon('film-strip')}</span>` : ""}</span>
        <span class="meta">${formatDuration(s.durationSec)} · ${formatBytes(s.sizeBytes)}</span>
      </div>
      <div class="row-actions">
        <button class="row-btn" data-action="play" title="Play">${icon('play')}</button>
        <button class="row-btn editor-only" data-action="add-mv" title="Attach music video">${icon('film-strip')}</button>
        <button class="row-btn editor-only" data-action="rename" title="Rename song">${icon('pencil-simple')}</button>
        <button class="row-btn editor-only" data-action="del" title="Delete song">${icon('trash')}</button>
      </div>`;
    li.addEventListener("click", (e) => {
      const a = e.target.closest("[data-action]")?.dataset?.action;
      if (a === "play") { e.stopPropagation(); playSong(s, songs); }
      else if (a === "add-mv") { e.stopPropagation(); openAttachMVModal(s); }
      else if (a === "rename") { e.stopPropagation(); openRenameSong(s); }
      else if (a === "del") {
        e.stopPropagation();
        if (!confirm(`Move "${s.title}" to trash?`)) return;
        App.DeleteSong(s.relPath).then(() => selectAlbum(artist, album)).catch((err) => alert(String(err)));
      } else {
        playSong(s, songs);
      }
    });
    songList.appendChild(li);
  }
  // Wire album buttons.
  $("play-album-btn").onclick = () => playQueue(songs, 0, false);
  $("shuffle-album-btn").onclick = () => playQueue(songs, 0, true);
}

// ---- Music player --------------------------------------------------------

function ensureAudioElement() {
  if (state.music.audio) return state.music.audio;
  const audio = new Audio();
  audio.preload = "metadata";
  audio.addEventListener("ended", onSongEnded);
  audio.addEventListener("play", () => { state.music.paused = false; renderNowPlaying(); });
  audio.addEventListener("pause", () => { state.music.paused = true; renderNowPlaying(); });
  audio.addEventListener("timeupdate", renderNowPlayingProgress);
  state.music.audio = audio;
  return audio;
}

function playSong(song, contextSongs = null) {
  // If launched from an album, use that album's order as the queue.
  // If just a single-song play, queue is [song].
  const queue = contextSongs && contextSongs.length > 0 ? contextSongs : [song];
  const idx = queue.findIndex((s) => s.relPath === song.relPath);
  playQueue(queue, idx >= 0 ? idx : 0, false);
}

function playQueue(songs, startIndex, shuffle) {
  if (!songs || songs.length === 0) return;
  let queue = songs.slice();
  if (shuffle) {
    queue = shuffleArray(queue);
  }
  state.music.queue = queue;
  state.music.queueIndex = shuffle ? 0 : startIndex;
  state.music.shuffle = shuffle;
  loadAndPlayCurrent();
}

function loadAndPlayCurrent() {
  const audio = ensureAudioElement();
  const song = state.music.queue[state.music.queueIndex];
  if (!song) return;
  audio.src = `/audio/${encodePath(song.relPath)}`;
  audio.play().catch(() => {});
  renderNowPlaying();
}

function onSongEnded() {
  if (state.music.queueIndex < state.music.queue.length - 1) {
    state.music.queueIndex++;
    loadAndPlayCurrent();
  } else {
    state.music.paused = true;
    renderNowPlaying();
  }
}

function shuffleArray(arr) {
  const a = arr.slice();
  for (let i = a.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

function renderNowPlaying() {
  const targets = [$("now-playing"), $("music-popout-body")];
  const song = state.music.queue[state.music.queueIndex];
  for (const t of targets) {
    if (!t) continue;
    if (!song) {
      t.className = "now-playing-empty";
      t.innerHTML = "<p>Nothing playing</p>";
      continue;
    }
    t.className = "now-playing";
    const artURL = `/art/${encodePath(song.relPath)}?t=${Date.now()}`;
    t.innerHTML = `
      <div class="np-art"><img src="${artURL}" alt="" onerror="this.style.display='none'"></div>
      <div class="np-meta">
        <div class="np-title">${escapeHTML(song.title)}</div>
        <div class="np-sub">${escapeHTML(song.artist)} — ${escapeHTML(song.album)}</div>
      </div>
      <div class="np-progress"><div class="np-bar" id="np-bar-${t.id}"></div></div>
      <div class="np-controls">
        <button class="np-btn" data-np="prev" title="Previous">${icon('skip-back')}</button>
        <button class="np-btn np-play" data-np="toggle" title="Play / pause">${icon(state.music.paused ? 'play' : 'pause')}</button>
        <button class="np-btn" data-np="next" title="Next">${icon('skip-forward')}</button>
        <button class="np-btn${state.music.shuffle ? ' active' : ''}" data-np="shuffle" title="Toggle shuffle">${icon('shuffle')}</button>
        ${song.hasMV ? `<button class="np-btn" data-np="mv" title="Play music video">${icon('film-strip')}</button>` : ""}
        <button class="np-btn" data-np="popout" title="${state.music.popoutOpen ? "Close popout" : "Pop out"}">${icon(state.music.popoutOpen ? 'corners-in' : 'arrow-square-out')}</button>
      </div>`;
    t.querySelectorAll("[data-np]").forEach((btn) => {
      btn.addEventListener("click", (e) => onPlayerControl(btn.dataset.np, e));
    });
  }
}

function renderNowPlayingProgress() {
  const audio = state.music.audio;
  if (!audio || !audio.duration) return;
  const pct = (audio.currentTime / audio.duration) * 100;
  for (const id of ["np-bar-now-playing", "np-bar-music-popout-body"]) {
    const bar = $(id);
    if (bar) bar.style.width = pct + "%";
  }
}

function onPlayerControl(action, e) {
  e?.stopPropagation();
  const audio = state.music.audio;
  if (!audio) return;
  switch (action) {
    case "toggle":
      if (audio.paused) audio.play().catch(() => {}); else audio.pause();
      break;
    case "prev":
      if (audio.currentTime > 3) { audio.currentTime = 0; }
      else if (state.music.queueIndex > 0) { state.music.queueIndex--; loadAndPlayCurrent(); }
      break;
    case "next":
      if (state.music.queueIndex < state.music.queue.length - 1) { state.music.queueIndex++; loadAndPlayCurrent(); }
      break;
    case "shuffle":
      state.music.shuffle = !state.music.shuffle;
      if (state.music.shuffle) {
        const cur = state.music.queue[state.music.queueIndex];
        const rest = state.music.queue.filter((_, i) => i !== state.music.queueIndex);
        state.music.queue = [cur, ...shuffleArray(rest)];
        state.music.queueIndex = 0;
      }
      renderNowPlaying();
      break;
    case "mv": {
      const song = state.music.queue[state.music.queueIndex];
      if (!song?.hasMV) return;
      // Music videos live as a sidecar mp4 next to the m4a. Pause
      // audio first so we don't play song + video together, then
      // route through the video player overlay with a Music-tree
      // URL. We don't go through playVideo() because position
      // bookmarks aren't a thing for music videos (a song's audio
      // already has its own queue position) — but we DO need to
      // mark playerCurrent=null so closePlayer doesn't try to save
      // a position to a bogus relPath.
      audio.pause();
      const mvRel = song.relPath.replace(/\.[^.]+$/, ".mp4");
      state.playerCurrent = null;        // no resume bookmarks for MVs
      stopPositionSaver();               // ditto — no periodic saves
      playerVideo.src = `/audio/${encodePath(mvRel)}`;
      playerTitle.textContent = `${song.artist} — ${song.title}`;
      player.classList.remove("hidden");
      playerVideo.play().catch(() => {});
      break;
    }
    case "popout":
      state.music.popoutOpen = !state.music.popoutOpen;
      $("music-popout").classList.toggle("hidden", !state.music.popoutOpen);
      renderNowPlaying();
      break;
  }
}

$("music-popout-close").addEventListener("click", () => {
  state.music.popoutOpen = false;
  $("music-popout").classList.add("hidden");
  renderNowPlaying();
});

// ===========================================================================
// MUSIC: attach music video modal
// ===========================================================================

let pendingMVSong = null;

function openAttachMVModal(song) {
  pendingMVSong = song;
  $("addvideo-modal-title").textContent = `Attach music video to "${song.title}"`;
  $("addvideo-hint").textContent = "Paste a YouTube URL — the resulting MP4 is attached to this song.";
  $("addvideo-urls").value = "";
  $("addvideo-quality-row").classList.remove("hidden");
  $("addvideo-fetch").classList.add("hidden");
  $("addvideo-manifest").classList.add("hidden");
  $("addvideo-start").classList.remove("hidden");
  $("addvideo-start").textContent = "Download + attach";
  // Show the log strip so MV download progress / error output is
  // visible — without this the MV flow runs silently and a failed
  // download has nowhere to surface its yt-dlp / muxer message.
  $("addvideo-log").classList.remove("hidden");
  $("addvideo-log").textContent = "";
  showModal($("addvideo-modal"));
  setTimeout(() => $("addvideo-urls").focus(), 50);
}

// ===========================================================================
// MUSIC: add-music modal
// ===========================================================================

$("add-music-btn").addEventListener("click", () => openAddDownloadsModal("music"));
$("add-video-btn").addEventListener("click", () => openAddDownloadsModal("video"));
$("add-channel-btn").addEventListener("click", openAddChannel);
$("add-folder-btn").addEventListener("click", openAddFolder);

function openAddDownloadsModal(kind) {
  pendingMVSong = null;
  if (kind === "music") {
    $("addvideo-modal-title").textContent = "Add music";
    $("addvideo-hint").textContent = "Paste YouTube Music song or album URLs (one per line). Tracks land under Music/Artist/Album/.";
    $("addvideo-quality-row").classList.add("hidden");
  } else {
    if (!state.selectedChannel) { alert("Select a channel first."); return; }
    if (state.currentFolder) {
      $("addvideo-modal-title").innerHTML =
        `Add videos to ${escapeHTML(state.selectedChannel)} / ${icon('folder')} ${escapeHTML(state.currentFolder)}`;
    } else {
      $("addvideo-modal-title").textContent = `Add videos to ${state.selectedChannel}`;
    }
    $("addvideo-hint").textContent = "Paste YouTube URLs — playlists create a folder under the channel and download every entry.";
    $("addvideo-quality-row").classList.remove("hidden");
  }
  $("addvideo-urls").value = "";
  $("addvideo-fetch").classList.remove("hidden");
  $("addvideo-manifest").classList.add("hidden");
  $("addvideo-manifest").innerHTML = "";
  $("addvideo-start").classList.add("hidden");
  $("addvideo-start").textContent = "Start downloading";
  $("addvideo-log").classList.add("hidden");
  $("addvideo-log").textContent = "";
  $("addvideo-modal").dataset.kind = kind;
  showModal($("addvideo-modal"));
  setTimeout(() => $("addvideo-urls").focus(), 50);
}

// ---- Manifest pre-fetch + per-item download progress --------------------

let downloadBusy = false;
let manifestEntries = [];

$("addvideo-fetch").addEventListener("click", async () => {
  const urls = $("addvideo-urls").value.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
  if (urls.length === 0) {
    $("addvideo-log").classList.remove("hidden");
    $("addvideo-log").textContent = "Paste at least one URL.";
    return;
  }
  $("addvideo-fetch").disabled = true;
  $("addvideo-fetch").textContent = "Fetching titles…";
  try {
    manifestEntries = await App.FetchDownloadManifest(urls);
    renderManifestList();
    $("addvideo-manifest").classList.remove("hidden");
    $("addvideo-start").classList.remove("hidden");
  } catch (err) {
    alert(`Couldn't fetch titles: ${err}`);
  } finally {
    $("addvideo-fetch").disabled = false;
    $("addvideo-fetch").textContent = "List items";
  }
});

function renderManifestList() {
  const ul = $("addvideo-manifest");
  ul.innerHTML = "";
  for (const entry of manifestEntries) {
    // Defensive: Go nil slices used to serialize as `null`. Backend
    // is fixed to send `[]` now, but coerce here so any future
    // regression doesn't crash the flow with `null.length`.
    const items = entry.items || [];
    const li = document.createElement("li");
    li.className = "manifest-entry";
    const itemsHTML = items.length > 1
      ? `<ul class="manifest-items">` +
        items.map((it, i) => `
          <li class="manifest-item" data-key="${escapeHTML(entry.url)}::${i}">
            <span class="mi-title">${escapeHTML(it.title)}</span>
            <div class="mi-progress"><div class="mi-bar"></div></div>
          </li>`).join("") + `</ul>`
      : `<div class="manifest-item" data-key="${escapeHTML(entry.url)}::0">
           <span class="mi-title">${escapeHTML(entry.title)}</span>
           <div class="mi-progress"><div class="mi-bar"></div></div>
         </div>`;
    li.innerHTML = `<div class="manifest-head">${icon(entry.kind === "playlist" ? "folder" : "play")} ${escapeHTML(entry.title)} <span class="muted">(${items.length} item${items.length === 1 ? "" : "s"})</span></div>${itemsHTML}`;
    ul.appendChild(li);
  }
}

$("addvideo-start").addEventListener("click", async () => {
  if (downloadBusy) return;
  downloadBusy = true;
  $("addvideo-start").disabled = true;
  $("addvideo-fetch").disabled = true;
  $("addvideo-cancel").disabled = true;
  $("addvideo-log").classList.remove("hidden");
  $("addvideo-log").textContent = "";

  const kind = $("addvideo-modal").dataset.kind;
  const urls = $("addvideo-urls").value.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
  try {
    if (pendingMVSong) {
      await App.AddMusicVideo(pendingMVSong.relPath, urls[0], $("addvideo-quality").value);
      pendingMVSong = null;
      hideModal($("addvideo-modal"));
      await renderMusicTab();
    } else if (kind === "music") {
      await App.AddMusic(urls);
      hideModal($("addvideo-modal"));
      await renderMusicTab();
    } else {
      await App.AddVideos(state.selectedChannel, state.currentFolder || "", urls, $("addvideo-quality").value);
      state.itemsByKey.clear();
      hideModal($("addvideo-modal"));
      await renderVideosTab();
    }
  } catch (err) {
    appendDownloadLog(`✗ ${err}`);
  } finally {
    downloadBusy = false;
    $("addvideo-start").disabled = false;
    $("addvideo-fetch").disabled = false;
    $("addvideo-cancel").disabled = false;
  }
});

$("addvideo-cancel").addEventListener("click", () => {
  if (downloadBusy) return;
  hideModal($("addvideo-modal"));
});
$("addvideo-close").addEventListener("click", () => {
  if (downloadBusy) return;
  hideModal($("addvideo-modal"));
});

function onYtdlpProgress(data) {
  if (!data) return;
  if (data.phase === "log" && data.line) {
    appendDownloadLog(data.line);
    // Try to advance per-item progress bars by matching log lines
    // to the manifest entries. yt-dlp emits "[download]  X.X% of …"
    // lines we can scrape.
    const m = /\[download\]\s+([\d.]+)%/.exec(data.line);
    if (m && data.url) {
      // Update the current playlist's currently-active item (we don't
      // know the index per-line, so just update the first non-100%
      // item under that URL).
      const entry = manifestEntries.find((e) => e.url === data.url);
      if (entry) {
        const items = $("addvideo-manifest").querySelectorAll(`[data-key^="${cssEscape(data.url)}::"]`);
        for (const node of items) {
          const bar = node.querySelector(".mi-bar");
          const cur = parseFloat(bar.style.width) || 0;
          if (cur < 100) {
            bar.style.width = m[1] + "%";
            break;
          }
        }
      }
    }
  } else if (data.phase === "done" && data.url) {
    // Mark all items under that URL as 100%.
    const items = $("addvideo-manifest").querySelectorAll(`[data-key^="${cssEscape(data.url)}::"]`);
    for (const node of items) {
      node.querySelector(".mi-bar").style.width = "100%";
    }
  } else if (data.phase === "error") {
    appendDownloadLog(`✗ ${data.url}: ${data.error}`);
  }
}

function appendDownloadLog(line) {
  const log = $("addvideo-log");
  const max = 200;
  const lines = log.textContent.split("\n");
  lines.push(line);
  if (lines.length > max) lines.splice(0, lines.length - max);
  log.textContent = lines.join("\n");
  log.scrollTop = log.scrollHeight;
}

function cssEscape(s) {
  return s.replace(/["\\]/g, "\\$&");
}

// ===========================================================================
// VIDEOS: existing modals (channel / folder / move / rename / confirm / thumb)
// ===========================================================================

const thumbModal = $("thumb-modal");
function openThumbModal(v) {
  $("thumb-url-input").value = "";
  setStatusEl($("thumb-modal-status"), "");
  showModal(thumbModal);
  setTimeout(() => $("thumb-url-input").focus(), 50);
}
$("thumb-modal-close").addEventListener("click", () => hideModal(thumbModal));
bindBackdropClose(thumbModal);

$("thumb-fetch-btn").addEventListener("click", async () => {
  const v = state.selectedItem;
  if (!v) return;
  const input = $("thumb-url-input").value.trim();
  if (!input) { setStatusEl($("thumb-modal-status"), "Paste a YouTube URL or video ID.", "error"); return; }
  setStatusEl($("thumb-modal-status"), "Fetching from YouTube…", "info");
  try {
    await App.FetchThumbnailFromYouTube(v.relPath, input);
    setStatusEl($("thumb-modal-status"), "Saved.", "ok");
    await renderDetail(v);
    setTimeout(() => hideModal(thumbModal), 600);
  } catch (err) { setStatusEl($("thumb-modal-status"), String(err), "error"); }
});
$("thumb-import-btn").addEventListener("click", async () => {
  const v = state.selectedItem; if (!v) return;
  try {
    const file = await OpenFileDialog({
      Title: "Choose a thumbnail image",
      Filters: [{ DisplayName: "Images", Pattern: "*.jpg;*.jpeg;*.png;*.webp" }],
    });
    if (!file) return;
    await App.ImportThumbnailFromFile(v.relPath, file);
    setStatusEl($("thumb-modal-status"), "Saved.", "ok");
    await renderDetail(v);
    setTimeout(() => hideModal(thumbModal), 600);
  } catch (err) { setStatusEl($("thumb-modal-status"), String(err), "error"); }
});
$("thumb-clear-btn").addEventListener("click", async () => {
  const v = state.selectedItem; if (!v) return;
  try { await App.ClearThumbnail(v.relPath); await renderDetail(v); hideModal(thumbModal); }
  catch (err) { setStatusEl($("thumb-modal-status"), String(err), "error"); }
});

// Add channel modal
function openAddChannel() {
  $("channel-modal-title").textContent = "New channel";
  $("channel-name-input").value = "";
  setStatusEl($("channel-modal-status"), "");
  showModal($("channel-modal"));
  setTimeout(() => $("channel-name-input").focus(), 50);
}
$("channel-save-btn").addEventListener("click", async () => {
  const name = $("channel-name-input").value.trim();
  if (!name) { setStatusEl($("channel-modal-status"), "Enter a name.", "error"); return; }
  try {
    await App.CreateChannel(name);
    hideModal($("channel-modal"));
    state.itemsByKey.clear();
    await renderVideosTab();
  } catch (err) { setStatusEl($("channel-modal-status"), String(err), "error"); }
});
$("channel-cancel-btn").addEventListener("click", () => hideModal($("channel-modal")));
$("channel-modal-close").addEventListener("click", () => hideModal($("channel-modal")));
bindBackdropClose($("channel-modal"));
$("channel-name-input").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); $("channel-save-btn").click(); }});

// Add folder modal
function openAddFolder() {
  if (!state.selectedChannel) { alert("Select a channel first."); return; }
  $("folder-modal-channel").textContent = state.selectedChannel;
  $("folder-name-input").value = "";
  setStatusEl($("folder-modal-status"), "");
  showModal($("folder-modal"));
  setTimeout(() => $("folder-name-input").focus(), 50);
}
$("folder-save-btn").addEventListener("click", async () => {
  const name = $("folder-name-input").value.trim();
  if (!name) { setStatusEl($("folder-modal-status"), "Enter a name.", "error"); return; }
  try {
    await App.CreateFolder(state.selectedChannel, name);
    hideModal($("folder-modal"));
    state.itemsByKey.clear();
    state.currentFolder = null;
    await renderVideosTab();
  } catch (err) { setStatusEl($("folder-modal-status"), String(err), "error"); }
});
$("folder-cancel-btn").addEventListener("click", () => hideModal($("folder-modal")));
$("folder-modal-close").addEventListener("click", () => hideModal($("folder-modal")));
bindBackdropClose($("folder-modal"));
$("folder-name-input").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); $("folder-save-btn").click(); }});

// Move modal
async function openMoveVideo(v) {
  $("move-modal-name").textContent = v.name;
  $("move-modal-channel").textContent = v.channel;
  setStatusEl($("move-modal-status"), "");
  $("move-dest-list").innerHTML = "<li class=\"move-dest-loading\">Loading…</li>";
  showModal($("move-modal"));
  let items = [];
  try { items = await App.Items(v.channel, ""); } catch (e) { alert(e); return; }
  const folders = items.filter((it) => it.kind === "folder").map((it) => it.name);
  $("move-dest-list").innerHTML = "";
  if (v.folder) {
    const li = document.createElement("li");
    li.className = "move-dest-row";
    li.innerHTML = `${icon('arrow-left')} Channel root`;
    li.addEventListener("click", () => doMove(v, ""));
    $("move-dest-list").appendChild(li);
  }
  for (const f of folders) {
    if (f === v.folder) continue;
    const li = document.createElement("li");
    li.className = "move-dest-row";
    li.innerHTML = `${icon('folder')} ${escapeHTML(f)}`;
    li.addEventListener("click", () => doMove(v, f));
    $("move-dest-list").appendChild(li);
  }
  if ($("move-dest-list").children.length === 0) {
    $("move-dest-list").innerHTML = `<li class="move-dest-empty">No other folders in this channel.</li>`;
  }
}
async function doMove(v, folder) {
  setStatusEl($("move-modal-status"), "Moving…", "info");
  try { await App.MoveVideo(v.relPath, folder); hideModal($("move-modal")); state.itemsByKey.clear(); await renderVideosTab(); }
  catch (err) { setStatusEl($("move-modal-status"), String(err), "error"); }
}
$("move-cancel-btn").addEventListener("click", () => hideModal($("move-modal")));
$("move-modal-close").addEventListener("click", () => hideModal($("move-modal")));
bindBackdropClose($("move-modal"));

// Rename modal
function openRenameChannel(name) {
  state.renameTarget = { kind: "channel", currentName: name };
  $("rename-modal-title").textContent = "Rename channel";
  $("rename-modal-hint").textContent = `Enter a new name for "${name}".`;
  openRenameModal(name);
}
function openRenameFolder(name) {
  state.renameTarget = { kind: "folder", currentName: name, channel: state.selectedChannel };
  $("rename-modal-title").textContent = "Rename folder";
  $("rename-modal-hint").textContent = `Enter a new name for folder "${name}".`;
  openRenameModal(name);
}
function openRenameVideo(v) {
  state.renameTarget = { kind: "video", currentName: v.name, relPath: v.relPath };
  $("rename-modal-title").textContent = "Rename video";
  $("rename-modal-hint").textContent = "New name (extension is preserved):";
  openRenameModal(v.name);
}
function openRenameArtist(name) {
  state.renameTarget = { kind: "artist", currentName: name };
  $("rename-modal-title").textContent = "Rename artist";
  $("rename-modal-hint").textContent = `Renames the artist's folder. Their albums and songs come along.`;
  openRenameModal(name);
}
function openRenameAlbum(artist, album) {
  state.renameTarget = { kind: "album", currentName: album, artist };
  $("rename-modal-title").textContent = "Rename album";
  $("rename-modal-hint").textContent = `Album by ${artist}.`;
  openRenameModal(album);
}
function openRenameSong(song) {
  state.renameTarget = { kind: "song", currentName: song.title, relPath: song.relPath, artist: song.artist, album: song.album };
  $("rename-modal-title").textContent = "Rename song";
  $("rename-modal-hint").textContent = "New name (extension is preserved):";
  openRenameModal(song.title);
}
function openRenameModal(value) {
  $("rename-input").value = value;
  setStatusEl($("rename-status"), "");
  showModal($("rename-modal"));
  setTimeout(() => { $("rename-input").focus(); $("rename-input").select(); }, 50);
}
$("rename-save-btn").addEventListener("click", async () => {
  const t = state.renameTarget; if (!t) return;
  const newName = $("rename-input").value.trim();
  if (!newName) { setStatusEl($("rename-status"), "Name cannot be empty.", "error"); return; }
  if (newName === t.currentName) { hideModal($("rename-modal")); return; }
  try {
    if (t.kind === "channel") {
      await App.RenameChannel(t.currentName, newName);
      if (state.selectedChannel === t.currentName) state.selectedChannel = newName;
      state.itemsByKey.clear();
      hideModal($("rename-modal"));
      await renderVideosTab();
    } else if (t.kind === "folder") {
      await App.RenameFolder(t.channel, t.currentName, newName);
      if (state.currentFolder === t.currentName) state.currentFolder = newName;
      state.itemsByKey.clear();
      hideModal($("rename-modal"));
      await renderVideosTab();
    } else if (t.kind === "video") {
      await App.RenameVideo(t.relPath, newName);
      state.itemsByKey.clear();
      hideModal($("rename-modal"));
      await renderVideosTab();
    } else if (t.kind === "artist") {
      await App.RenameArtist(t.currentName, newName);
      // If the user was browsing this artist's albums, update the
      // selected-artist pointer so the expansion stays open after
      // the rescan.
      if (state.selectedArtist === t.currentName) state.selectedArtist = newName;
      if (state.expandedArtists.has(t.currentName)) {
        state.expandedArtists.delete(t.currentName);
        state.expandedArtists.add(newName);
      }
      hideModal($("rename-modal"));
      await renderMusicTab();
    } else if (t.kind === "album") {
      await App.RenameAlbum(t.artist, t.currentName, newName);
      if (state.selectedAlbum === t.currentName) state.selectedAlbum = newName;
      hideModal($("rename-modal"));
      await renderMusicTab();
      if (state.selectedArtist && state.selectedAlbum) {
        await selectAlbum(state.selectedArtist, state.selectedAlbum);
      }
    } else if (t.kind === "song") {
      await App.RenameSong(t.relPath, newName);
      hideModal($("rename-modal"));
      await renderMusicTab();
      if (state.selectedArtist && state.selectedAlbum) {
        await selectAlbum(state.selectedArtist, state.selectedAlbum);
      }
    }
  } catch (err) { setStatusEl($("rename-status"), String(err), "error"); }
});
$("rename-cancel-btn").addEventListener("click", () => hideModal($("rename-modal")));
$("rename-modal-close").addEventListener("click", () => hideModal($("rename-modal")));
bindBackdropClose($("rename-modal"));
$("rename-input").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); $("rename-save-btn").click(); }});

// Confirm modal
function openConfirm(opts) {
  $("confirm-title").textContent = opts.title;
  $("confirm-message").textContent = opts.message;
  $("confirm-hint").innerHTML = opts.hint || `Moves to <code>.trash/</code> — recoverable until you delete that folder manually.`;
  const yes = $("confirm-yes"); yes.textContent = opts.primary.label; yes.className = opts.primary.kind || "danger-primary";
  state.confirmAction = opts.primary.action;
  const alt = $("confirm-yes-alt");
  if (opts.alt) {
    alt.textContent = opts.alt.label;
    alt.className = opts.alt.kind || "";
    alt.classList.remove("hidden");
    state.confirmAltAction = opts.alt.action;
  } else {
    alt.classList.add("hidden");
    state.confirmAltAction = null;
  }
  showModal($("confirm-modal"));
}
function confirmDeleteChannel(name, count) {
  openConfirm({
    title: "Delete channel",
    message: `Move "${name}" and its ${count} video${count === 1 ? "" : "s"} to .trash?`,
    primary: { label: "Move to trash", kind: "danger-primary", action: async () => {
      try { await App.DeleteChannel(name); state.itemsByKey.clear(); state.selectedChannel = null; hideModal($("confirm-modal")); await renderVideosTab(); }
      catch (err) { alert(String(err)); }
    }},
  });
}
function confirmDeleteVideo(v) {
  openConfirm({
    title: "Delete video", message: `Move "${v.name}" to .trash?`,
    primary: { label: "Move to trash", kind: "danger-primary", action: async () => {
      try { await App.DeleteVideo(v.relPath); state.itemsByKey.clear(); hideModal($("confirm-modal")); await renderVideosTab(); }
      catch (err) { alert(String(err)); }
    }},
  });
}
function confirmDeleteFolder(name, count) {
  openConfirm({
    title: "Empty or delete folder",
    message: `"${name}" contains ${count} video${count === 1 ? "" : "s"}.`,
    hint: `<b>Empty</b> moves the videos to the channel root and removes the folder. <b>Delete</b> moves the folder and everything to <code>.trash</code>.`,
    primary: { label: "Delete folder + videos", kind: "danger-primary", action: async () => {
      try { await App.DeleteFolder(state.selectedChannel, name); state.itemsByKey.clear();
        if (state.currentFolder === name) state.currentFolder = null;
        hideModal($("confirm-modal")); await renderVideosTab(); }
      catch (err) { alert(String(err)); }
    }},
    alt: { label: "Empty (keep videos)", kind: "primary", action: async () => {
      try { await App.EmptyFolder(state.selectedChannel, name); state.itemsByKey.clear();
        if (state.currentFolder === name) state.currentFolder = null;
        hideModal($("confirm-modal")); await renderVideosTab(); }
      catch (err) { alert(String(err)); }
    }},
  });
}
$("confirm-yes").addEventListener("click", async () => { const a = state.confirmAction; state.confirmAction = null; state.confirmAltAction = null; if (a) await a(); });
$("confirm-yes-alt").addEventListener("click", async () => { const a = state.confirmAltAction; state.confirmAction = null; state.confirmAltAction = null; if (a) await a(); });
$("confirm-no").addEventListener("click", () => hideModal($("confirm-modal")));
$("confirm-close").addEventListener("click", () => hideModal($("confirm-modal")));
bindBackdropClose($("confirm-modal"));

// ===========================================================================
// EDIT CAPABILITY (yt-dlp + online check; only meaningful for Editor)
// ===========================================================================

async function refreshEditCapability(forceServerCheck) {
  state.editCap = forceServerCheck ? await App.RefreshEditCapability() : await App.GetEditCapability();
  if (state.editCap?.reason === "Preparing yt-dlp…") {
    clearTimeout(state.preparingPoll);
    state.preparingPoll = setTimeout(() => refreshEditCapability(true), 1000);
  } else if (state.preparingPoll) {
    clearTimeout(state.preparingPoll);
    state.preparingPoll = null;
  }
}

// ===========================================================================
// HELPERS
// ===========================================================================

function showModal(m) { m.classList.remove("hidden"); }
function hideModal(m) { m.classList.add("hidden"); }
function setStatusEl(el, text, kind) { el.textContent = text || ""; el.className = `modal-hint modal-status ${kind || ""}`; }
function highlightSelection(listEl, attrName, value) {
  for (const li of listEl.children) li.classList.toggle("selected", li.dataset[attrName] === value);
}
function bindBackdropClose(m) {
  let downOnBackdrop = false;
  m.addEventListener("mousedown", (e) => { downOnBackdrop = e.target === m && !m.classList.contains("modal-blocking"); });
  m.addEventListener("click", (e) => { if (e.target === m && downOnBackdrop) hideModal(m); downOnBackdrop = false; });
}
function formatDuration(sec) {
  if (!sec || !isFinite(sec)) return "—";
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  return h ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}` : `${m}:${String(s).padStart(2, "0")}`;
}
function formatBytes(n) {
  if (!n) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}
function escapeHTML(s) {
  return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}
function encodePath(relPath) { return relPath.split("/").map(encodeURIComponent).join("/"); }

// icon renders a Phosphor Bold SVG by sprite ID. The sprite lives at
// frontend/src/icons.svg (run tools/fetch-icons.sh to regenerate);
// the Wails asset server serves it from src/icons.svg. CSS sets
// `fill: currentColor` so theming via the parent's text color just
// works.
function icon(name) {
  return `<svg class="icon" aria-hidden="true"><use href="src/icons.svg#ph-${name}"/></svg>`;
}

// Global keyboard
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    if (!player.classList.contains("hidden")) { closePlayer(); return; }
    if (state.music.popoutOpen) {
      state.music.popoutOpen = false;
      $("music-popout").classList.add("hidden");
      renderNowPlaying();
      return;
    }
    for (const id of ["confirm-modal", "rename-modal", "addvideo-modal", "channel-modal", "folder-modal", "move-modal", "thumb-modal", "add-account-modal"]) {
      const m = $(id);
      if (m && !m.classList.contains("hidden") && !m.classList.contains("modal-blocking")) {
        if (id === "addvideo-modal" && downloadBusy) return;
        hideModal(m); return;
      }
    }
  }
  if (e.key === "f" && !player.classList.contains("hidden") && document.activeElement?.tagName !== "INPUT") {
    e.preventDefault(); toggleFullscreen();
  }
});

init();
