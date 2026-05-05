// YTDisc — frontend logic.
//
// Backend bindings (Wails-generated): window.go.main.App.<Method>(...).
// All async. Wails event API on window.runtime.

const {
  Status,
  Channels,
  Items,
  HasThumbnail,
  FetchThumbnailFromYouTube,
  ImportThumbnailFromFile,
  ClearThumbnail,
  GetEditCapability,
  RefreshEditCapability,
  CreateChannel,
  RenameChannel,
  DeleteChannel,
  CreateFolder,
  RenameFolder,
  DeleteFolder,
  EmptyFolder,
  MoveVideo,
  RenameVideo,
  DeleteVideo,
  AddVideos,
  GetPosition,
  SavePosition,
  ClearPosition,
} = window.go.main.App;

const { OpenFileDialog, EventsOn } = window.runtime;

// ---- DOM refs ------------------------------------------------------------

const $ = (id) => document.getElementById(id);

const channelList = $("channel-list");
const videoList = $("video-list");
const videosHeader = $("videos-header");
const detail = $("detail");
const statusStats = $("status-stats");
const editToggle = $("edit-toggle");

const player = $("player");
const playerVideo = $("player-video");
const playerTitle = $("player-title");
const playerClose = $("player-close");
const playerFullscreen = $("player-fullscreen");

// Modals
const thumbModal = $("thumb-modal");
const channelModal = $("channel-modal");
const addvideoModal = $("addvideo-modal");
const renameModal = $("rename-modal");
const confirmModal = $("confirm-modal");
const folderModal = $("folder-modal");
const moveModal = $("move-modal");

// ---- State ---------------------------------------------------------------

let state = {
  selectedChannel: null, // string
  currentFolder: null,   // string|null — null = at channel root
  selectedItem: null,    // {kind:"folder"|"video", ...}
  itemsByKey: new Map(), // cache key = "channel/folder?", value = items[]
  editing: false,
  editCap: null,         // EditCapability snapshot
  // Pending actions for shared modals.
  renameTarget: null,    // {kind:"channel"|"folder"|"video", currentName, ...}
  confirmAction: null,   // function — primary button
  confirmAltAction: null,// function|null — secondary button
};

// State for the resume-playback feature. We track the currently playing
// video so the periodic save / beforeunload handlers know what to write.
const player_state = {
  current: null,         // VideoInfo currently in the <video> element
  saveTimer: null,
  resuming: false,       // suppress save during the seek-to-saved-pos jump
};
const RESUME_HEAD_SECS = 45;  // skip resume if saved pos < this many seconds in
const RESUME_TAIL_SECS = 20;  // skip resume if pos is within this many seconds of end
const POSITION_SAVE_INTERVAL_MS = 5000;

// ---- Init ----------------------------------------------------------------

async function init() {
  const status = await Status();
  if (!status.ok) {
    renderNoLibrary(status.message);
    return;
  }
  renderStats(status);

  await refreshEditCapability(false);

  const channels = await Channels();
  renderChannels(channels);

  if (channels.length > 0) {
    selectChannel(channels[0].name);
  }

  // Re-check edit capability when the window regains focus — covers
  // the "I just plugged in ethernet / installed yt-dlp" case without
  // forcing the user to restart.
  window.addEventListener("focus", () => refreshEditCapability(true));

  // Listen for yt-dlp progress events from the backend.
  EventsOn("ytdlp-progress", onYtdlpProgress);

  // Last-ditch save when the window/app is going away. beforeunload
  // fires for ⌘W on macOS Wails and for app quit when the window is
  // the last one open. Force-quits and crashes obviously bypass this.
  window.addEventListener("beforeunload", () => savePlayerPosition({ flushOnly: true }));
  window.addEventListener("pagehide", () => savePlayerPosition({ flushOnly: true }));
}

// ---- Library rendering ---------------------------------------------------

function renderNoLibrary(msg) {
  $("app").innerHTML = `
    <div class="message">
      <div>
        <p>${escapeHTML(msg || "No Videos/ folder found.")}</p>
        <p>Place this app on a USB stick next to a folder named
        <code>Videos/</code> with one subfolder per channel.</p>
      </div>
    </div>`;
}

function renderChannels(channels) {
  channelList.innerHTML = "";
  for (const c of channels) {
    const li = document.createElement("li");
    li.dataset.channel = c.name;
    li.innerHTML = `
      <div class="row-main">
        <span class="title">${escapeHTML(c.name)}</span>
        <span class="meta">${c.videoCount} video${
      c.videoCount === 1 ? "" : "s"
    } · ${formatDuration(c.totalSecs)}</span>
      </div>
      <div class="row-actions edit-only">
        <button class="row-btn" data-action="rename-channel" title="Rename">✎</button>
        <button class="row-btn" data-action="delete-channel" title="Delete">🗑</button>
      </div>`;
    li.addEventListener("click", (e) => {
      const action = e.target.closest("[data-action]")?.dataset?.action;
      if (action === "rename-channel") {
        e.stopPropagation();
        openRenameChannel(c.name);
      } else if (action === "delete-channel") {
        e.stopPropagation();
        confirmDeleteChannel(c.name, c.videoCount);
      } else {
        selectChannel(c.name);
      }
    });
    channelList.appendChild(li);
  }
}

async function selectChannel(name) {
  state.selectedChannel = name;
  state.currentFolder = null;
  state.selectedItem = null;
  highlightSelection(channelList, "channel", name);
  videosHeader.textContent = name;
  detail.className = "detail-empty";
  detail.innerHTML = "<p>Select a video</p>";
  await loadAndRenderItems();
}

// loadAndRenderItems (re)fetches the items for the currently selected
// channel + folder and renders them in the videos column.
async function loadAndRenderItems() {
  const ch = state.selectedChannel;
  if (!ch) return;
  const key = itemsKey(ch, state.currentFolder);
  let items = state.itemsByKey.get(key);
  if (!items) {
    items = await Items(ch, state.currentFolder || "");
    state.itemsByKey.set(key, items);
  }
  videosHeader.textContent = state.currentFolder
    ? `${ch} / ${state.currentFolder}`
    : ch;
  renderItems(items);
}

function itemsKey(channel, folder) {
  return folder ? `${channel}\0${folder}` : channel;
}

function renderItems(items) {
  videoList.innerHTML = "";

  // When inside a folder, prepend a "← Back" row so the user can
  // climb back out without losing the channel selection.
  if (state.currentFolder) {
    const back = document.createElement("li");
    back.className = "back-row";
    back.innerHTML = `
      <div class="row-main">
        <span class="title">← Back to ${escapeHTML(state.selectedChannel)}</span>
      </div>`;
    back.addEventListener("click", () => {
      state.currentFolder = null;
      state.selectedItem = null;
      detail.className = "detail-empty";
      detail.innerHTML = "<p>Select a video</p>";
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
          <span class="title">📁 ${escapeHTML(it.name)}</span>
          <span class="meta">${it.videoCount} video${
        it.videoCount === 1 ? "" : "s"
      } · ${formatDuration(it.totalSecs)}</span>
        </div>
        <div class="row-actions edit-only">
          <button class="row-btn" data-action="rename-folder" title="Rename">✎</button>
          <button class="row-btn" data-action="delete-folder" title="Empty / delete">🗑</button>
        </div>`;
      li.addEventListener("click", (e) => {
        const action = e.target.closest("[data-action]")?.dataset?.action;
        if (action === "rename-folder") {
          e.stopPropagation();
          openRenameFolder(it.name);
        } else if (action === "delete-folder") {
          e.stopPropagation();
          confirmDeleteFolder(it.name, it.videoCount);
        } else {
          enterFolder(it.name);
        }
      });
    } else {
      li.dataset.relpath = it.relPath;
      li.innerHTML = `
        <div class="row-main">
          <span class="title">${escapeHTML(it.name)}</span>
          <span class="meta">${formatDuration(it.totalSecs)}${
        it.width ? ` · ${it.width}×${it.height}` : ""
      }</span>
        </div>
        <div class="row-actions edit-only">
          <button class="row-btn" data-action="move-video" title="Move to folder">↪</button>
          <button class="row-btn" data-action="rename-video" title="Rename">✎</button>
          <button class="row-btn" data-action="delete-video" title="Delete">🗑</button>
        </div>`;
      li.addEventListener("click", (e) => {
        const action = e.target.closest("[data-action]")?.dataset?.action;
        if (action === "move-video") {
          e.stopPropagation();
          openMoveVideo(it);
        } else if (action === "rename-video") {
          e.stopPropagation();
          openRenameVideo(it);
        } else if (action === "delete-video") {
          e.stopPropagation();
          confirmDeleteVideo(it);
        } else {
          selectVideo(it);
        }
      });
    }
    videoList.appendChild(li);
  }
}

function enterFolder(name) {
  state.currentFolder = name;
  state.selectedItem = null;
  detail.className = "detail-empty";
  detail.innerHTML = "<p>Select a video</p>";
  loadAndRenderItems();
}

async function selectVideo(v) {
  state.selectedItem = v;
  highlightSelection(videoList, "relpath", v.relPath);
  await renderDetail(v);
}

async function renderDetail(v) {
  const has = await HasThumbnail(v.relPath);
  const thumbURL = has
    ? `/thumb/${encodePath(v.relPath)}?t=${Date.now()}`
    : null;

  // Check for a saved resume position so the play button can hint it.
  let resumeAt = 0;
  try {
    resumeAt = await GetPosition(v.relPath);
  } catch (_) { /* ignore */ }
  const willResume = shouldResumeAt(resumeAt, v.totalSecs);

  detail.className = "detail";
  detail.innerHTML = `
    <div class="detail-thumb">
      ${
        thumbURL
          ? `<img src="${thumbURL}" alt="">`
          : `<div class="placeholder">No thumbnail</div>`
      }
      <div class="duration-tag">${formatDuration(v.totalSecs)}</div>
    </div>
    <h1 class="detail-title">${escapeHTML(v.name)}</h1>
    <div class="detail-channel">${escapeHTML(v.channel)}${
    v.folder ? ` · 📁 ${escapeHTML(v.folder)}` : ""
  }</div>
    <div class="detail-stats">
      ${v.width ? `<span>${v.width} × ${v.height}</span>` : ""}
      <span>${formatBytes(v.sizeBytes)}</span>
    </div>
    <div class="detail-actions">
      <button class="play-btn" id="play-btn">${
        willResume ? `▶ Resume at ${formatDuration(resumeAt)}` : "▶ Play"
      }</button>
      ${
        willResume
          ? `<button class="secondary-btn" id="play-from-start-btn">From start</button>`
          : ""
      }
      <button class="secondary-btn" id="set-thumb-btn">
        ${has ? "Change thumbnail…" : "Set thumbnail…"}
      </button>
    </div>`;

  $("play-btn").addEventListener("click", () => play(v));
  $("play-from-start-btn")?.addEventListener("click", () => play(v, { fromStart: true }));
  $("set-thumb-btn").addEventListener("click", () => openThumbModal(v));
}

// ---- Player + fullscreen + resume ---------------------------------------

// shouldResumeAt encodes the product rule: resume only if the saved
// position is past the first 45 seconds AND before the last 20 seconds.
function shouldResumeAt(pos, durationSec) {
  if (!pos || pos <= 0) return false;
  if (pos < RESUME_HEAD_SECS) return false;
  if (durationSec && pos > durationSec - RESUME_TAIL_SECS) return false;
  return true;
}

async function play(v, opts = {}) {
  player_state.current = v;
  player_state.resuming = false;
  playerVideo.src = `/video/${encodePath(v.relPath)}`;
  playerTitle.textContent = `${v.channel} — ${v.name}`;
  player.classList.remove("hidden");

  // Decide whether to resume. Skip the lookup entirely on "From start".
  let resumeAt = 0;
  if (!opts.fromStart) {
    try {
      resumeAt = await GetPosition(v.relPath);
    } catch (_) { /* ignore */ }
  }
  if (shouldResumeAt(resumeAt, v.totalSecs)) {
    // Wait for metadata so currentTime= is honored. The "loadedmetadata"
    // event fires reliably across browsers; "canplay" can be late.
    player_state.resuming = true;
    const seek = () => {
      try {
        playerVideo.currentTime = resumeAt;
      } catch (_) { /* ignore */ }
      // Tiny delay so the seek's own timeupdate doesn't immediately
      // overwrite the saved position with the resume point itself.
      setTimeout(() => { player_state.resuming = false; }, 200);
      playerVideo.removeEventListener("loadedmetadata", seek);
    };
    playerVideo.addEventListener("loadedmetadata", seek);
  }

  startPositionSaver();
  playerVideo.play().catch(() => {});
}

function closePlayer() {
  // Persist where we were before we tear down the <video>.
  savePlayerPosition();
  stopPositionSaver();

  // If we're currently fullscreen, exit first so we don't leave the
  // user staring at a blank screen.
  if (document.fullscreenElement) {
    document.exitFullscreen?.();
  }
  player.classList.add("hidden");
  playerVideo.pause();
  playerVideo.removeAttribute("src");
  playerVideo.load();
  // Refresh the detail panel so the play button reflects the new
  // saved position immediately ("▶ Resume at …").
  if (state.selectedItem && state.selectedItem.kind === "video") {
    renderDetail(state.selectedItem);
  }
  player_state.current = null;
}

function toggleFullscreen() {
  if (document.fullscreenElement) {
    document.exitFullscreen?.();
    return;
  }
  if (playerVideo.requestFullscreen) {
    playerVideo.requestFullscreen().catch(() => {});
  } else if (playerVideo.webkitRequestFullscreen) {
    playerVideo.webkitRequestFullscreen();
  } else if (playerVideo.webkitEnterFullscreen) {
    playerVideo.webkitEnterFullscreen();
  }
}

playerClose.addEventListener("click", closePlayer);
playerFullscreen.addEventListener("click", toggleFullscreen);

document.addEventListener("fullscreenchange", () => {
  player.classList.toggle("is-fullscreen", !!document.fullscreenElement);
});

// When playback runs to completion, drop the saved position so the
// next "▶ Play" doesn't try to resume to within the last 20 seconds.
playerVideo.addEventListener("ended", () => {
  const v = player_state.current;
  if (v) {
    ClearPosition(v.relPath).catch(() => {});
  }
});

playerVideo.addEventListener("pause", () => savePlayerPosition());

// ---- Position-save plumbing ---------------------------------------------

function startPositionSaver() {
  stopPositionSaver();
  player_state.saveTimer = setInterval(() => {
    savePlayerPosition();
  }, POSITION_SAVE_INTERVAL_MS);
}

function stopPositionSaver() {
  if (player_state.saveTimer) {
    clearInterval(player_state.saveTimer);
    player_state.saveTimer = null;
  }
}

// savePlayerPosition writes the current playhead to the backend. Skips
// writing during the resume-seek itself (so we don't immediately
// overwrite the saved position with the same value), and skips when
// the playhead is essentially at zero or already at the end.
function savePlayerPosition({ flushOnly = false } = {}) {
  const v = player_state.current;
  if (!v) return;
  if (player_state.resuming && !flushOnly) return;
  const pos = playerVideo.currentTime;
  if (!isFinite(pos) || pos <= 1) {
    // Treat near-zero as "not started" — nothing useful to bookmark.
    return;
  }
  // If we're within the last 20 s, treat that as "essentially done"
  // and clear the bookmark so the next play starts fresh, matching
  // the resume rule on the read side.
  if (v.totalSecs && pos > v.totalSecs - RESUME_TAIL_SECS) {
    ClearPosition(v.relPath).catch(() => {});
    return;
  }
  SavePosition(v.relPath, pos).catch(() => {});
}

// ---- Thumbnail modal -----------------------------------------------------

const thumbUrlInput = $("thumb-url-input");
const thumbFetchBtn = $("thumb-fetch-btn");
const thumbImportBtn = $("thumb-import-btn");
const thumbClearBtn = $("thumb-clear-btn");
const thumbModalStatus = $("thumb-modal-status");

function openThumbModal(v) {
  thumbUrlInput.value = "";
  setStatusEl(thumbModalStatus, "");
  showModal(thumbModal);
  setTimeout(() => thumbUrlInput.focus(), 50);
}

$("thumb-modal-close").addEventListener("click", () => hideModal(thumbModal));
bindBackdropClose(thumbModal);

async function fetchFromYouTube() {
  const v = currentVideo();
  if (!v) return;
  const input = thumbUrlInput.value.trim();
  if (!input) {
    setStatusEl(thumbModalStatus, "Paste a YouTube URL or video ID.", "error");
    return;
  }
  setStatusEl(thumbModalStatus, "Fetching from YouTube…", "info");
  thumbFetchBtn.disabled = true;
  try {
    await FetchThumbnailFromYouTube(v.relPath, input);
    setStatusEl(thumbModalStatus, "Saved.", "ok");
    await renderDetail(v);
    setTimeout(() => hideModal(thumbModal), 600);
  } catch (err) {
    setStatusEl(thumbModalStatus, String(err), "error");
  } finally {
    thumbFetchBtn.disabled = false;
  }
}

async function importFromFile() {
  const v = currentVideo();
  if (!v) return;
  try {
    const file = await OpenFileDialog({
      Title: "Choose a thumbnail image",
      Filters: [
        {
          DisplayName: "Images (*.jpg, *.jpeg, *.png, *.webp)",
          Pattern: "*.jpg;*.jpeg;*.png;*.webp",
        },
      ],
    });
    if (!file) return;
    setStatusEl(thumbModalStatus, "Importing…", "info");
    await ImportThumbnailFromFile(v.relPath, file);
    setStatusEl(thumbModalStatus, "Saved.", "ok");
    await renderDetail(v);
    setTimeout(() => hideModal(thumbModal), 600);
  } catch (err) {
    setStatusEl(thumbModalStatus, String(err), "error");
  }
}

async function clearThumbnail() {
  const v = currentVideo();
  if (!v) return;
  try {
    await ClearThumbnail(v.relPath);
    await renderDetail(v);
    hideModal(thumbModal);
  } catch (err) {
    setStatusEl(thumbModalStatus, String(err), "error");
  }
}

function currentVideo() {
  const it = state.selectedItem;
  return it && it.kind === "video" ? it : null;
}

thumbFetchBtn.addEventListener("click", fetchFromYouTube);
thumbImportBtn.addEventListener("click", importFromFile);
thumbClearBtn.addEventListener("click", clearThumbnail);
thumbUrlInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    fetchFromYouTube();
  }
});

// ---- Add channel modal ---------------------------------------------------

const channelNameInput = $("channel-name-input");
const channelStatus = $("channel-modal-status");

function openAddChannel() {
  $("channel-modal-title").textContent = "New channel";
  channelNameInput.value = "";
  setStatusEl(channelStatus, "");
  showModal(channelModal);
  setTimeout(() => channelNameInput.focus(), 50);
}

async function saveChannel() {
  const name = channelNameInput.value.trim();
  if (!name) {
    setStatusEl(channelStatus, "Enter a channel name.", "error");
    return;
  }
  try {
    await CreateChannel(name);
    hideModal(channelModal);
    state.itemsByKey.clear();
    await reloadAfterMutation(name, null);
  } catch (err) {
    setStatusEl(channelStatus, String(err), "error");
  }
}

$("add-channel-btn").addEventListener("click", openAddChannel);
$("channel-save-btn").addEventListener("click", saveChannel);
$("channel-cancel-btn").addEventListener("click", () => hideModal(channelModal));
$("channel-modal-close").addEventListener("click", () => hideModal(channelModal));
bindBackdropClose(channelModal);
channelNameInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    saveChannel();
  }
});

// ---- Add folder modal ----------------------------------------------------

const folderNameInput = $("folder-name-input");
const folderStatus = $("folder-modal-status");

function openAddFolder() {
  if (!state.selectedChannel) {
    alert("Select a channel first.");
    return;
  }
  $("folder-modal-channel").textContent = state.selectedChannel;
  folderNameInput.value = "";
  setStatusEl(folderStatus, "");
  showModal(folderModal);
  setTimeout(() => folderNameInput.focus(), 50);
}

async function saveFolder() {
  const name = folderNameInput.value.trim();
  if (!name) {
    setStatusEl(folderStatus, "Enter a folder name.", "error");
    return;
  }
  try {
    await CreateFolder(state.selectedChannel, name);
    hideModal(folderModal);
    state.itemsByKey.clear();
    // Stay at the channel root so the user can see the new folder.
    state.currentFolder = null;
    await reloadAfterMutation(state.selectedChannel, null);
  } catch (err) {
    setStatusEl(folderStatus, String(err), "error");
  }
}

$("add-folder-btn").addEventListener("click", openAddFolder);
$("folder-save-btn").addEventListener("click", saveFolder);
$("folder-cancel-btn").addEventListener("click", () => hideModal(folderModal));
$("folder-modal-close").addEventListener("click", () => hideModal(folderModal));
bindBackdropClose(folderModal);
folderNameInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    saveFolder();
  }
});

// ---- Add videos modal (yt-dlp) -------------------------------------------

const addvideoUrls = $("addvideo-urls");
const addvideoStart = $("addvideo-start");
const addvideoCancel = $("addvideo-cancel");
const addvideoTarget = $("addvideo-target");
const addvideoQuality = $("addvideo-quality");
const addvideoProgress = $("addvideo-progress");
const addvideoSummary = $("addvideo-summary");
const addvideoLog = $("addvideo-log");

let addvideoBusy = false;

function openAddVideos() {
  if (!state.selectedChannel) {
    alert("Select a channel first to add videos to.");
    return;
  }
  // Show the destination — channel root, or "Channel / Folder" when
  // we're currently inside a folder. Downloads land there (playlists
  // still split into a new folder under the channel root).
  addvideoTarget.textContent = state.currentFolder
    ? `${state.selectedChannel} / 📁 ${state.currentFolder}`
    : state.selectedChannel;
  addvideoUrls.value = "";
  addvideoQuality.value = "fhd";
  addvideoProgress.classList.add("hidden");
  addvideoLog.textContent = "";
  addvideoSummary.textContent = "";
  addvideoStart.disabled = false;
  addvideoCancel.textContent = "Close";
  showModal(addvideoModal);
  setTimeout(() => addvideoUrls.focus(), 50);
}

async function startAddVideos() {
  if (addvideoBusy) return;
  const lines = addvideoUrls.value
    .split(/\r?\n/)
    .map((s) => s.trim())
    .filter(Boolean);
  if (lines.length === 0) {
    addvideoSummary.textContent = "Paste at least one URL.";
    addvideoProgress.classList.remove("hidden");
    return;
  }

  addvideoBusy = true;
  addvideoStart.disabled = true;
  addvideoUrls.disabled = true;
  addvideoQuality.disabled = true;
  addvideoProgress.classList.remove("hidden");
  addvideoLog.textContent = "";
  addvideoSummary.textContent = "Starting…";

  const destFolder = state.currentFolder || "";
  try {
    await AddVideos(state.selectedChannel, destFolder, lines, addvideoQuality.value);
    addvideoSummary.textContent = `✓ Done — ${lines.length} URL${
      lines.length === 1 ? "" : "s"
    } processed.`;
    addvideoCancel.textContent = "Done";
    state.itemsByKey.clear();
    await reloadAfterMutation(state.selectedChannel, state.currentFolder);
  } catch (err) {
    addvideoSummary.textContent = `✗ Error: ${String(err)}`;
  } finally {
    addvideoBusy = false;
    addvideoStart.disabled = false;
    addvideoUrls.disabled = false;
    addvideoQuality.disabled = false;
  }
}

function onYtdlpProgress(data) {
  if (!data) return;
  if (data.phase === "starting") {
    addvideoSummary.textContent = `[${data.current}/${data.total}] Downloading: ${data.url}`;
  } else if (data.phase === "done") {
    addvideoSummary.textContent = `[${data.current}/${data.total}] ✓ Finished: ${data.url}`;
  } else if (data.phase === "error") {
    addvideoSummary.textContent = `[${data.current}/${data.total}] ✗ ${data.url}`;
  } else if (data.phase === "all-done") {
    addvideoSummary.textContent = `✓ All ${data.total} done.`;
  } else if (data.phase === "log" && data.line) {
    appendLog(data.line);
  }
}

function appendLog(line) {
  // Cap log size so a 4-hour download doesn't accumulate megabytes
  // of progress lines in the DOM.
  const max = 200;
  const lines = addvideoLog.textContent.split("\n");
  lines.push(line);
  if (lines.length > max) lines.splice(0, lines.length - max);
  addvideoLog.textContent = lines.join("\n");
  addvideoLog.scrollTop = addvideoLog.scrollHeight;
}

$("add-video-btn").addEventListener("click", openAddVideos);
addvideoStart.addEventListener("click", startAddVideos);
addvideoCancel.addEventListener("click", () => {
  if (addvideoBusy) return; // can't close mid-download
  hideModal(addvideoModal);
});
$("addvideo-close").addEventListener("click", () => {
  if (addvideoBusy) return;
  hideModal(addvideoModal);
});

// ---- Move-to-folder modal -----------------------------------------------

const moveDestList = $("move-dest-list");
const moveModalStatus = $("move-modal-status");

let moveTarget = null; // {relPath, name, folder} of the video being moved

async function openMoveVideo(v) {
  moveTarget = { relPath: v.relPath, name: v.name, folder: v.folder || "" };
  $("move-modal-name").textContent = v.name;
  $("move-modal-channel").textContent = v.channel;
  setStatusEl(moveModalStatus, "");
  moveDestList.innerHTML = "<li class=\"move-dest-loading\">Loading…</li>";
  showModal(moveModal);

  // Fetch the channel's items so we can list the available folders.
  // Always re-fetch (don't read from itemsByKey) — folder list might
  // have changed since the user entered the channel.
  let items;
  try {
    items = await Items(v.channel, "");
  } catch (err) {
    setStatusEl(moveModalStatus, String(err), "error");
    moveDestList.innerHTML = "";
    return;
  }
  const folders = items.filter((it) => it.kind === "folder").map((it) => it.name);

  moveDestList.innerHTML = "";
  // "Channel root" as a destination, hidden if we're already there.
  if (moveTarget.folder !== "") {
    moveDestList.appendChild(buildMoveDestRow("", "↩ Channel root"));
  }
  for (const f of folders) {
    if (f === moveTarget.folder) continue; // already there
    moveDestList.appendChild(buildMoveDestRow(f, `📁 ${f}`));
  }
  if (moveDestList.children.length === 0) {
    moveDestList.innerHTML = `<li class="move-dest-empty">No other folders in this channel — create one with the 📁＋ button.</li>`;
  }
}

function buildMoveDestRow(folder, label) {
  const li = document.createElement("li");
  li.className = "move-dest-row";
  li.textContent = label;
  li.addEventListener("click", async () => {
    if (!moveTarget) return;
    setStatusEl(moveModalStatus, "Moving…", "info");
    try {
      await MoveVideo(moveTarget.relPath, folder);
      hideModal(moveModal);
      state.itemsByKey.clear();
      // If we were inside the source folder and that folder is now
      // empty, the user stays inside it — that's fine, the items list
      // will re-render empty. If they were at root, the moved video
      // disappears. Either way, refresh in place.
      await reloadAfterMutation(state.selectedChannel, state.currentFolder);
    } catch (err) {
      setStatusEl(moveModalStatus, String(err), "error");
    }
  });
  return li;
}

$("move-cancel-btn").addEventListener("click", () => hideModal(moveModal));
$("move-modal-close").addEventListener("click", () => hideModal(moveModal));
bindBackdropClose(moveModal);

// ---- Rename modal --------------------------------------------------------

const renameInput = $("rename-input");
const renameStatus = $("rename-status");

function openRenameChannel(name) {
  state.renameTarget = { kind: "channel", currentName: name };
  $("rename-modal-title").textContent = "Rename channel";
  $("rename-modal-hint").textContent = `Enter a new name for "${name}".`;
  renameInput.value = name;
  setStatusEl(renameStatus, "");
  showModal(renameModal);
  setTimeout(() => {
    renameInput.focus();
    renameInput.select();
  }, 50);
}

function openRenameFolder(name) {
  state.renameTarget = {
    kind: "folder",
    currentName: name,
    channel: state.selectedChannel,
  };
  $("rename-modal-title").textContent = "Rename folder";
  $("rename-modal-hint").textContent = `Enter a new name for folder "${name}".`;
  renameInput.value = name;
  setStatusEl(renameStatus, "");
  showModal(renameModal);
  setTimeout(() => {
    renameInput.focus();
    renameInput.select();
  }, 50);
}

function openRenameVideo(v) {
  state.renameTarget = {
    kind: "video",
    currentName: v.name,
    relPath: v.relPath,
  };
  $("rename-modal-title").textContent = "Rename video";
  $("rename-modal-hint").textContent = "New name (extension is preserved):";
  renameInput.value = v.name;
  setStatusEl(renameStatus, "");
  showModal(renameModal);
  setTimeout(() => {
    renameInput.focus();
    renameInput.select();
  }, 50);
}

async function saveRename() {
  const target = state.renameTarget;
  if (!target) return;
  const newName = renameInput.value.trim();
  if (!newName) {
    setStatusEl(renameStatus, "Name cannot be empty.", "error");
    return;
  }
  if (newName === target.currentName) {
    hideModal(renameModal);
    return;
  }

  try {
    if (target.kind === "channel") {
      await RenameChannel(target.currentName, newName);
      state.itemsByKey.clear();
      hideModal(renameModal);
      await reloadAfterMutation(newName, null);
    } else if (target.kind === "folder") {
      await RenameFolder(target.channel, target.currentName, newName);
      state.itemsByKey.clear();
      // If the user was inside the renamed folder, follow the rename.
      const nextFolder = state.currentFolder === target.currentName ? newName : state.currentFolder;
      hideModal(renameModal);
      await reloadAfterMutation(state.selectedChannel, nextFolder);
    } else {
      await RenameVideo(target.relPath, newName);
      state.itemsByKey.clear();
      hideModal(renameModal);
      await reloadAfterMutation(state.selectedChannel, state.currentFolder);
    }
  } catch (err) {
    setStatusEl(renameStatus, String(err), "error");
  }
}

$("rename-save-btn").addEventListener("click", saveRename);
$("rename-cancel-btn").addEventListener("click", () => hideModal(renameModal));
$("rename-modal-close").addEventListener("click", () => hideModal(renameModal));
bindBackdropClose(renameModal);
renameInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    saveRename();
  }
});

// ---- Confirm modal -------------------------------------------------------

// openConfirm renders the shared confirm modal. opts:
//   title, message, hint?
//   primary: { label, kind: "danger-primary"|"primary", action }
//   alt?:    { label, kind, action }   — second button next to primary
function openConfirm(opts) {
  $("confirm-title").textContent = opts.title;
  $("confirm-message").textContent = opts.message;
  $("confirm-hint").innerHTML = opts.hint || `Moves to <code>Videos/.trash/</code> — recoverable until you delete that folder manually.`;

  const yes = $("confirm-yes");
  yes.textContent = opts.primary.label;
  yes.className = `${opts.primary.kind || "danger-primary"}`;
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

  showModal(confirmModal);
}

function confirmDeleteChannel(name, count) {
  const word = count === 1 ? "video" : "videos";
  openConfirm({
    title: "Delete channel",
    message: `Move "${name}" and its ${count} ${word} to .trash?`,
    primary: {
      label: "Move to trash",
      kind: "danger-primary",
      action: async () => {
        try {
          await DeleteChannel(name);
          state.itemsByKey.clear();
          if (state.selectedChannel === name) {
            state.selectedChannel = null;
            state.currentFolder = null;
          }
          hideModal(confirmModal);
          await reloadAfterMutation(null, null);
        } catch (err) {
          alert(String(err));
        }
      },
    },
  });
}

function confirmDeleteVideo(v) {
  openConfirm({
    title: "Delete video",
    message: `Move "${v.name}" to .trash?`,
    primary: {
      label: "Move to trash",
      kind: "danger-primary",
      action: async () => {
        try {
          await DeleteVideo(v.relPath);
          state.itemsByKey.clear();
          hideModal(confirmModal);
          await reloadAfterMutation(state.selectedChannel, state.currentFolder);
        } catch (err) {
          alert(String(err));
        }
      },
    },
  });
}

function confirmDeleteFolder(name, count) {
  const word = count === 1 ? "video" : "videos";
  openConfirm({
    title: "Empty or delete folder",
    message: `"${name}" contains ${count} ${word}.`,
    hint: `<b>Empty</b> moves the videos out to the channel root and removes the folder. <b>Delete</b> moves the folder and everything inside to <code>.trash</code>.`,
    primary: {
      label: "Delete folder + videos",
      kind: "danger-primary",
      action: async () => {
        try {
          await DeleteFolder(state.selectedChannel, name);
          state.itemsByKey.clear();
          // If we happened to be inside that folder, climb out.
          if (state.currentFolder === name) state.currentFolder = null;
          hideModal(confirmModal);
          await reloadAfterMutation(state.selectedChannel, state.currentFolder);
        } catch (err) {
          alert(String(err));
        }
      },
    },
    alt: {
      label: "Empty (keep videos)",
      kind: "primary",
      action: async () => {
        try {
          await EmptyFolder(state.selectedChannel, name);
          state.itemsByKey.clear();
          if (state.currentFolder === name) state.currentFolder = null;
          hideModal(confirmModal);
          await reloadAfterMutation(state.selectedChannel, state.currentFolder);
        } catch (err) {
          alert(String(err));
        }
      },
    },
  });
}

$("confirm-yes").addEventListener("click", async () => {
  const action = state.confirmAction;
  state.confirmAction = null;
  state.confirmAltAction = null;
  if (action) await action();
});
$("confirm-yes-alt").addEventListener("click", async () => {
  const action = state.confirmAltAction;
  state.confirmAction = null;
  state.confirmAltAction = null;
  if (action) await action();
});
$("confirm-no").addEventListener("click", () => hideModal(confirmModal));
$("confirm-close").addEventListener("click", () => hideModal(confirmModal));
bindBackdropClose(confirmModal);

// ---- Edit mode toggle ----------------------------------------------------

async function refreshEditCapability(forceServerCheck) {
  state.editCap = forceServerCheck
    ? await RefreshEditCapability()
    : await GetEditCapability();
  renderEditToggle();
}

function renderEditToggle() {
  const cap = state.editCap;
  if (!cap) {
    editToggle.textContent = "Loading…";
    editToggle.disabled = true;
    return;
  }

  if (state.editing) {
    editToggle.textContent = "✓ Done editing";
    editToggle.disabled = false;
    editToggle.classList.add("active");
    editToggle.classList.remove("disabled");
    document.body.classList.add("edit-mode");
    return;
  }

  document.body.classList.remove("edit-mode");
  editToggle.classList.remove("active");

  if (cap.enabled) {
    editToggle.textContent = "✎ Edit";
    editToggle.disabled = false;
    editToggle.classList.remove("disabled");
    editToggle.title = `yt-dlp ${cap.ytDlpVersion || ""} · online`;
  } else {
    editToggle.textContent = `✎ ${cap.reason}`;
    editToggle.classList.add("disabled");
    editToggle.disabled = false;
    editToggle.title = "Click to recheck";
  }
}

editToggle.addEventListener("click", async () => {
  const cap = state.editCap;
  if (!cap) return;

  if (state.editing) {
    state.editing = false;
    renderEditToggle();
    return;
  }

  if (!cap.enabled) {
    editToggle.textContent = "Checking…";
    editToggle.disabled = true;
    await refreshEditCapability(true);
    return;
  }

  state.editing = true;
  renderEditToggle();
});

// ---- Reload helpers ------------------------------------------------------

// reloadAfterMutation refreshes the channel list and reselects either
// the named channel or, if it's gone, the first available one. If a
// folder is also passed, drills into it after the channel reload.
async function reloadAfterMutation(channelToSelect, folderToSelect) {
  const status = await Status();
  if (status.ok) renderStats(status);

  const channels = await Channels();
  renderChannels(channels);

  let target = channelToSelect;
  if (!target || !channels.find((c) => c.name === target)) {
    target = channels[0]?.name || null;
  }

  if (target) {
    await selectChannel(target);
    if (folderToSelect) {
      // selectChannel resets to root; drill back in if requested. Make
      // sure the folder still exists — if it was renamed/deleted we
      // gracefully stay at the root.
      const items = await Items(target, "");
      if (items.find((it) => it.kind === "folder" && it.name === folderToSelect)) {
        enterFolder(folderToSelect);
      }
    }
  } else {
    state.selectedChannel = null;
    state.currentFolder = null;
    state.selectedItem = null;
    videosHeader.textContent = "Videos";
    videoList.innerHTML = "";
    detail.className = "detail-empty";
    detail.innerHTML = "<p>Select a video</p>";
  }
}

// ---- Global keyboard shortcuts -------------------------------------------

document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    if (!player.classList.contains("hidden")) {
      if (anyModalOpen()) return;
      closePlayer();
      return;
    }
    for (const m of [
      confirmModal,
      renameModal,
      addvideoModal,
      channelModal,
      folderModal,
      moveModal,
      thumbModal,
    ]) {
      if (!m.classList.contains("hidden")) {
        if (m === addvideoModal && addvideoBusy) return;
        hideModal(m);
        return;
      }
    }
  }
  if (e.key === "f" && !player.classList.contains("hidden")) {
    if (!anyModalOpen() && document.activeElement?.tagName !== "INPUT") {
      e.preventDefault();
      toggleFullscreen();
    }
  }
});

function anyModalOpen() {
  for (const m of [
    confirmModal,
    renameModal,
    addvideoModal,
    channelModal,
    folderModal,
    moveModal,
    thumbModal,
  ]) {
    if (!m.classList.contains("hidden")) return true;
  }
  return false;
}

// ---- Status bar ----------------------------------------------------------

function renderStats(status) {
  statusStats.textContent = `${status.channels} channel${
    status.channels === 1 ? "" : "s"
  } · ${status.videos} video${
    status.videos === 1 ? "" : "s"
  } · ${formatDuration(status.totalSecs)} · ${formatBytes(status.totalBytes)}`;
}

// ---- Helpers -------------------------------------------------------------

function showModal(m) {
  m.classList.remove("hidden");
}
function hideModal(m) {
  m.classList.add("hidden");
}

// bindBackdropClose closes the modal when the user clicks the dark
// backdrop area (everything outside the .modal-card). It guards
// against the "select-text-and-drag-out" case: a mousedown that
// starts inside the card and ends on the backdrop must NOT close the
// modal, because that would surprise users who drag-selected text in
// an input and released the mouse outside.
function bindBackdropClose(m) {
  let downOnBackdrop = false;
  m.addEventListener("mousedown", (e) => {
    downOnBackdrop = e.target === m;
  });
  m.addEventListener("click", (e) => {
    if (e.target === m && downOnBackdrop) {
      hideModal(m);
    }
    downOnBackdrop = false;
  });
}

function setStatusEl(el, text, kind) {
  el.textContent = text || "";
  el.className = `modal-hint modal-status ${kind || ""}`;
}

function highlightSelection(listEl, attrName, value) {
  for (const li of listEl.children) {
    li.classList.toggle("selected", li.dataset[attrName] === value);
  }
}

function formatDuration(sec) {
  if (!sec || !isFinite(sec)) return "—";
  sec = Math.round(sec);
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  return h
    ? `${h}:${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`
    : `${m}:${String(s).padStart(2, "0")}`;
}

function formatBytes(n) {
  if (!n) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

function escapeHTML(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function encodePath(relPath) {
  return relPath.split("/").map(encodeURIComponent).join("/");
}

// ---- Go ------------------------------------------------------------------

init();
