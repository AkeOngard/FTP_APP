import * as App from '../wailsjs/go/main/App.js';
import { EventsOn, OnFileDrop } from '../wailsjs/runtime/runtime.js';
import {
    h, clear, button, icon, toast, errText, confirmDialog, promptDialog, debounce,
    formatBytes, formatDate, formatDuration, joinRemote, parentRemote, joinLocal,
} from './util.js';

const DEFAULT_PORTS = { ftp: 21, ftps: 21, sftp: 22, tftp: 69 };
const PROTOCOLS = [['sftp', 'SFTP'], ['ftp', 'FTP'], ['ftps', 'FTPS (FTP over TLS)'], ['tftp', 'TFTP']];
const FORM_KEY = 'ftpapp.connection';

const state = {
    conn: null, // { id, protocol, caps, host, port, user }
    local: { path: '', parent: '', entries: [], sel: new Set(), anchor: -1 },
    remote: { path: '/', entries: [], sel: new Set(), anchor: -1 },
    transfers: new Map(),
};

const ui = {}; // DOM references
let sites = [];

export async function initClient(root) {
    ui.root = root;
    buildConnectionForm();
    buildConnectedBar();
    buildPanels();
    buildQueue();
    clear(root).append(ui.form, ui.connected, ui.panels, ui.queue);
    showConnectionState();

    loadFormFromStorage();
    await reloadSites();
    await navigate('local', '');

    EventsOn('transfer', onTransferEvent);
    const initial = (await App.GetTransfers()) || [];
    initial.forEach((t) => state.transfers.set(t.id, t));
    renderQueue();

    try {
        OnFileDrop((x, y, paths) => {
            const hit = document.elementFromPoint(x, y);
            if (state.conn && paths?.length && hit?.closest('#remote-list')) uploadPaths(paths, state.remote.path);
        }, true);
    } catch { /* not running inside the desktop shell */ }
}

// ====================================================================
// connection form
// ====================================================================

function buildConnectionForm() {
    const f = ui.f = {};
    f.site = h('select', { 'aria-label': 'Saved sites', onchange: onSitePicked });
    f.protocol = h('select', { value: 'sftp', onchange: updateProtocolUI }, PROTOCOLS.map(([v, l]) => h('option', { value: v }, l)));
    f.host = h('input', { type: 'text', placeholder: 'host name or IP', spellcheck: 'false', autocomplete: 'off' });
    f.port = h('input', { type: 'number', min: 1, max: 65535 });
    f.user = h('input', { type: 'text', spellcheck: 'false', autocomplete: 'off' });
    f.password = h('input', { type: 'password', autocomplete: 'off' });
    f.keyFile = h('input', { type: 'text', placeholder: 'optional private key file', spellcheck: 'false' });
    f.keyPass = h('input', { type: 'password', placeholder: 'key passphrase (if any)', autocomplete: 'off' });
    f.skipTls = h('input', { type: 'checkbox' });
    f.block = h('input', { type: 'number', min: 512, max: 65464, placeholder: '1468', style: 'width:110px' });
    f.parallel = h('input', { type: 'number', min: 1, max: 16, placeholder: '4', style: 'width:80px' });
    f.parallelField = h('label', {
        class: 'field',
        title: 'How many files are transferred at the same time, each on its own connection. More helps with many small files and ' +
            'slow or distant servers (200 small files over a 20 ms link: 1 connection 20 s, 8 connections 3 s); one big file is not split. ' +
            'Some servers allow only a few connections per user; the app backs off by itself if a server refuses more. Empty = 4.',
    }, h('span', {}, 'Parallel transfers'), f.parallel);
    f.blockField = h('label', {
        class: 'field',
        title: 'TFTP sends one block at a time, so bigger blocks are much faster (8192 is about 10x faster than 512 on a clean LAN). ' +
            'Leave empty for 1468, which fits one Ethernet frame and works everywhere. The server must support the size.',
    }, h('span', {}, 'Block size (bytes)'), f.block);

    const wrap = (label, el, cls = '') => h('label', { class: `field ${cls}` }, h('span', {}, label), el);
    f.userField = wrap('User', f.user);
    f.passField = wrap('Password', f.password);
    f.keyRow = h('div', { class: 'conn-row', style: 'grid-template-columns: 1fr 1fr auto' },
        wrap('Key file (SFTP)', h('div', { class: 'with-btn' }, f.keyFile,
            button('Browse…', { small: true, onClick: async () => { const p = await App.PickKeyFile(); if (p) f.keyFile.value = p; } }))),
        wrap('Key passphrase', f.keyPass),
        h('span'));
    f.tlsRow = h('label', { class: 'check' }, f.skipTls, 'Accept self-signed certificates (skip verification)');

    // The key file row is rarely needed, so it stays folded away until asked for.
    f.keyToggle = button('Use a key file…', { small: true, iconName: 'key', onClick: () => { keyOpen = !keyOpen; updateProtocolUI(); } });
    f.connect = button('Connect', { kind: 'primary', iconName: 'plug', onClick: doConnect });
    f.save = button('Save site…', { small: true, onClick: saveSite });
    f.delete = button('Delete site', { small: true, kind: 'danger', onClick: deleteSite });

    for (const el of [f.host, f.port, f.user, f.password]) {
        el.addEventListener('keydown', (e) => { if (e.key === 'Enter') doConnect(); });
    }

    ui.form = h('section', { class: 'card connbar' },
        h('div', { class: 'conn-row' },
            wrap('Saved sites', f.site), wrap('Protocol', f.protocol), wrap('Host', f.host),
            wrap('Port', f.port), f.userField, f.passField),
        f.keyRow,
        h('div', { class: 'conn-actions' }, f.connect, f.save, f.delete, f.keyToggle, f.parallelField, f.blockField, h('span', { class: 'grow' }), f.tlsRow),
    );
    updateProtocolUI();
}

let keyOpen = false; // whether the key file row is unfolded

function updateProtocolUI() {
    const p = ui.f.protocol.value;
    ui.f.port.placeholder = String(DEFAULT_PORTS[p]);
    ui.f.userField.hidden = ui.f.passField.hidden = p === 'tftp';
    ui.f.blockField.hidden = p !== 'tftp';
    ui.f.keyToggle.hidden = p !== 'sftp';
    ui.f.keyRow.hidden = p !== 'sftp' || !keyOpen;
    ui.f.tlsRow.hidden = p !== 'ftps';
}

function readParams() {
    const f = ui.f;
    const p = f.protocol.value;
    return {
        protocol: p,
        host: f.host.value.trim(),
        port: parseInt(f.port.value, 10) || 0,
        user: p === 'tftp' ? '' : f.user.value.trim(), // TFTP has no login; ignore a stale hidden field
        password: p === 'tftp' ? '' : f.password.value,
        keyFile: p === 'sftp' && keyOpen ? f.keyFile.value.trim() : '',
        keyPassphrase: p === 'sftp' && keyOpen ? f.keyPass.value : '',
        skipTlsVerify: p === 'ftps' && f.skipTls.checked,
        blockSize: p === 'tftp' ? parseInt(f.block.value, 10) || 0 : 0,
        parallel: parseInt(f.parallel.value, 10) || 0,
        timeoutSecs: 15,
    };
}

function writeParams(s) {
    const f = ui.f;
    f.protocol.value = s.protocol || 'sftp';
    f.host.value = s.host || '';
    f.port.value = s.port || '';
    f.user.value = s.user || '';
    f.keyFile.value = s.keyFile || '';
    f.skipTls.checked = !!s.skipTlsVerify;
    f.block.value = s.blockSize || '';
    f.parallel.value = s.parallel || '';
    f.password.value = '';
    f.keyPass.value = '';
    keyOpen = !!s.keyFile; // a site that uses a key shows it
    updateProtocolUI();
}

function saveFormToStorage(p) {
    try {
        const { password, keyPassphrase, ...safe } = p; // secrets are never persisted
        localStorage.setItem(FORM_KEY, JSON.stringify(safe));
    } catch { /* storage unavailable */ }
}

function loadFormFromStorage() {
    try {
        const raw = localStorage.getItem(FORM_KEY);
        if (raw) writeParams(JSON.parse(raw));
    } catch { /* ignore a corrupt value */ }
}

async function reloadSites(selectName) {
    sites = (await App.GetSites()) || [];
    const f = ui.f;
    clear(f.site).append(h('option', { value: '' }, sites.length ? 'Choose…' : 'None saved'),
        sites.map((s) => h('option', { value: s.name }, s.name)));
    f.site.value = selectName || '';
    f.delete.disabled = !selectName;
}

function onSitePicked() {
    const s = sites.find((x) => x.name === ui.f.site.value);
    ui.f.delete.disabled = !s;
    if (!s) return;
    writeParams(s);
    (s.protocol === 'tftp' ? ui.f.connect : ui.f.password).focus();
}

async function saveSite() {
    const p = readParams();
    if (!p.host) { toast('Enter a host first'); return; }
    const name = await promptDialog('Save site', 'Name', { initial: ui.f.site.value || p.host, ok: 'Save' });
    if (!name) return;
    try {
        await App.SaveSite({
            name: name.trim(), protocol: p.protocol, host: p.host, port: p.port, user: p.user,
            keyFile: p.keyFile, skipTlsVerify: p.skipTlsVerify, blockSize: p.blockSize, parallel: p.parallel,
        });
        await reloadSites(name.trim());
        toast('Site saved (the password is never stored)', 'good');
    } catch (e) { toast(errText(e), 'error'); }
}

async function deleteSite() {
    const name = ui.f.site.value;
    if (!name || !await confirmDialog('Delete site', `Delete the saved site "${name}"?`, { ok: 'Delete', danger: true })) return;
    try { await App.DeleteSite(name); await reloadSites(); } catch (e) { toast(errText(e), 'error'); }
}

async function doConnect() {
    const p = readParams();
    if (!p.host) { toast('Enter a host'); ui.f.host.focus(); return; }
    ui.f.connect.disabled = true;
    ui.f.connect.lastChild.textContent = 'Connecting…';
    try {
        let info = await App.Connect(p);
        if (info.unknownHost) {
            // First contact with this SFTP server: nothing was sent to it yet. Connect only if the user
            // accepts its key; the second Connect is refused unless the server presents that same key.
            if (!await confirmHostKey(info.unknownHost)) return;
            await App.TrustHost(info.unknownHost.host, info.unknownHost);
            info = await App.Connect(p);
            if (info.unknownHost) throw new Error('The server presented a different key the second time; not connecting.');
        }
        saveFormToStorage(p);
        state.conn = { ...info, host: p.host, port: p.port || DEFAULT_PORTS[p.protocol], user: p.user };
        state.remote = { path: info.cwd || '/', entries: [], sel: new Set(), anchor: -1 };
        ui.f.password.value = ui.f.keyPass.value = '';
        showConnectionState();
        if (info.caps.browse) await navigate('remote', state.remote.path);
    } catch (e) {
        await onConnectError(e, p);
    } finally {
        ui.f.connect.disabled = false;
        ui.f.connect.lastChild.textContent = 'Connect';
    }
}

function confirmHostKey(q) {
    return confirmDialog('Unknown server',
        [
            h('div', {}, `This is the first connection to ${q.host}. Its identity cannot be checked automatically.`),
            h('div', { class: 'dlg-key' },
                h('div', { class: 'muted' }, `Host key fingerprint (${q.keyType})`),
                h('code', {}, q.fingerprint)),
            h('div', {}, 'If you can, compare it with the fingerprint the server\'s administrator gives you. ' +
                'If it does not match, someone may be intercepting the connection: cancel.'),
            h('div', { class: 'muted' }, 'Once trusted, the app refuses to connect if this server ever shows a different key.'),
        ],
        { ok: 'Trust and connect' });
}

async function onConnectError(e, p) {
    const msg = errText(e);
    if (/host key for .* changed/.test(msg)) {
        const port = p.port || DEFAULT_PORTS[p.protocol];
        const ok = await confirmDialog('Host key changed',
            `${msg}\n\nOnly forget the old key if you know the server was reinstalled or its key was replaced; otherwise someone may be intercepting the connection.`,
            { ok: 'Forget old key', danger: true });
        if (ok) {
            await App.ForgetHost(p.host, port);
            toast('Old key forgotten. Connect again to trust the new one.');
        }
        return;
    }
    toast(msg, 'error');
}

async function doDisconnect() {
    const id = state.conn?.id;
    state.conn = null;
    state.remote = { path: '/', entries: [], sel: new Set(), anchor: -1 };
    showConnectionState();
    try { if (id) await App.Disconnect(id); } catch { /* already gone */ }
}

// ====================================================================
// connected bar + layout switching
// ====================================================================

function buildConnectedBar() {
    ui.connectedText = h('div', { class: 'grow' });
    ui.connected = h('section', { class: 'card connected-bar' },
        icon('plug'), ui.connectedText, button('Disconnect', { onClick: doDisconnect }));
}

function showConnectionState() {
    const c = state.conn;
    ui.form.hidden = !!c;
    ui.connected.hidden = !c;
    ui.panels.hidden = false;
    if (c) {
        ui.connectedText.textContent = `${c.protocol.toUpperCase()}  ·  ${c.user ? c.user + '@' : ''}${c.host}:${c.port}`;
    }
    const browse = !!c?.caps.browse;
    const tftp = !!c && !c.caps.browse;
    ui.remotePanel.hidden = !browse;
    ui.placeholder.hidden = !!c;
    ui.tftpCard.hidden = !tftp;
    ui.mid.hidden = !browse;
    ui.panels.style.gridTemplateColumns = browse ? '1fr auto 1fr' : '1fr 1fr';
    // the placeholder and the TFTP card take the slot the remote panel uses in browse mode
    ui.placeholder.style.gridColumn = ui.tftpCard.style.gridColumn = '2';
    ui.remoteModify.forEach((b) => { b.disabled = !c?.caps.modify; });
    renderList('remote');
    updateActionState();
}

// ====================================================================
// panels
// ====================================================================

function buildPanels() {
    const local = makePanel('local');
    const remote = makePanel('remote');
    ui.localPanel = local.el;
    ui.remotePanel = remote.el;
    ui.remoteModify = remote.modifyButtons;

    ui.btnUpload = button('Upload', { kind: 'primary', iconName: 'upload', title: 'Upload the selected local items', onClick: () => transferSelected('local') });
    ui.btnDownload = button('Download', { kind: 'primary', iconName: 'download', title: 'Download the selected remote items', onClick: () => transferSelected('remote') });
    ui.mid = h('div', { class: 'mid' }, ui.btnUpload, ui.btnDownload);

    ui.placeholder = h('section', { class: 'card panel' },
        h('div', { class: 'empty', style: 'margin:auto' }, 'Connect to a server to browse its files.'));

    ui.tftpName = h('input', { type: 'text', placeholder: 'e.g. firmware/image.bin', spellcheck: 'false' });
    ui.tftpDownload = button('Download to the local folder', {
        iconName: 'download',
        onClick: async () => {
            const name = ui.tftpName.value.trim();
            if (!name) { toast('Enter the remote file name'); return; }
            const local = { name: name.split(/[\\/]/).pop(), isDir: false };
            if (!await confirmOverwrite('local', state.local.path, [local])) return;
            await runTransfer(() => App.Download(state.conn.id, [{ name: local.name, path: name, isDir: false, size: -1 }], state.local.path));
            toast('Queued 1 item');
        },
    });
    ui.tftpUpload = button('Upload the selected local files', {
        iconName: 'upload', kind: 'primary',
        onClick: () => transferSelected('local'),
    });
    ui.tftpCard = h('section', { class: 'card tftp-card' },
        h('div', { class: 'svc-title' }, 'TFTP transfer'),
        h('div', { class: 'notice info' }, 'TFTP cannot list or manage files. Type the name of a file on the server to download it, or select local files and upload them.'),
        h('label', { class: 'field' }, h('span', {}, 'Remote file name'), ui.tftpName),
        ui.tftpDownload,
        ui.tftpUpload,
    );

    // grid order: local | mid | remote (browsable protocols), local | placeholder or TFTP card otherwise
    ui.panels = h('div', { class: 'panels' }, local.el, ui.mid, remote.el, ui.placeholder, ui.tftpCard);
}

function makePanel(side) {
    const isRemote = side === 'remote';
    const pathInput = h('input', { type: 'text', class: 'path', spellcheck: 'false', 'aria-label': `${side} path` });
    pathInput.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') navigate(side, pathInput.value.trim());
    });

    const upBtn = button('Up one level', { only: true, small: true, iconName: 'up', onClick: () => goUp(side) });
    const refreshBtn = button('Refresh', { only: true, small: true, iconName: 'refresh', onClick: () => refresh(side) });
    const mkdirBtn = button('New folder', { only: true, small: true, iconName: 'plus', onClick: () => makeFolder(side) });
    const modifyButtons = [mkdirBtn];
    const bar = [upBtn, pathInput, refreshBtn, mkdirBtn];

    if (isRemote) {
        const renameBtn = button('Rename', { only: true, small: true, iconName: 'rename', onClick: renameSelected });
        const deleteBtn = button('Delete', { only: true, small: true, iconName: 'trash', onClick: deleteSelected });
        modifyButtons.push(renameBtn, deleteBtn);
        bar.push(renameBtn, deleteBtn);
        ui.btnRename = renameBtn;
        ui.btnDelete = deleteBtn;
    } else {
        const roots = h('select', { 'aria-label': 'Drive or location', onchange: () => { if (roots.value) navigate('local', roots.value); roots.value = ''; } });
        ui.roots = roots;
        bar.unshift(roots);
        App.LocalRoots().then((rs) => {
            clear(roots).append(h('option', { value: '' }, 'Go to…'), h('option', { value: '~' }, 'Home'), (rs || []).map((r) => h('option', { value: r }, r)));
        });
    }

    const tbody = h('tbody');
    const list = h('div', { class: 'list', id: `${side}-list`, tabindex: 0 },
        h('table', { class: 'files' },
            h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', { class: 'size' }, 'Size'), h('th', { class: 'date' }, 'Modified'))),
            tbody),
    );
    const empty = h('div', { class: 'empty', hidden: true });
    list.append(empty);
    const foot = h('div', { class: 'panel-foot' });

    // internal drag and drop between the two panels
    list.addEventListener('dragover', (e) => {
        if (dragSide && dragSide !== side) { e.preventDefault(); list.classList.add('drop-hover'); }
    });
    list.addEventListener('dragleave', (e) => { if (e.target === list) list.classList.remove('drop-hover'); });
    list.addEventListener('drop', (e) => {
        list.classList.remove('drop-hover');
        if (dragSide && dragSide !== side) { e.preventDefault(); transferSelected(dragSide); }
    });
    list.addEventListener('keydown', (e) => onListKey(side, e));

    const el = h('section', { class: 'card panel' },
        h('div', { class: 'panel-title' }, isRemote ? 'Remote' : 'This computer'),
        h('div', { class: 'panel-bar' }, bar),
        list, foot);
    ui[side] = { tbody, pathInput, empty, foot, list, upBtn };
    return { el, modifyButtons };
}

let dragSide = null;

function modelOf(side) { return state[side]; }

async function navigate(side, target) {
    const m = modelOf(side);
    const before = { path: m.path, sel: m.sel };
    try {
        if (side === 'local') {
            const res = await App.ListLocal(target === '~' ? '' : target);
            Object.assign(m, { path: res.path, parent: res.parent, entries: res.entries || [] });
        } else {
            if (!state.conn) return;
            const entries = (await App.ListRemote(state.conn.id, target)) || [];
            Object.assign(m, { path: target, entries });
        }
        // A refresh of the same folder keeps the selection (minus items that disappeared).
        const names = new Set(m.entries.map((e) => e.name));
        m.sel = m.path === before.path ? new Set([...before.sel].filter((n) => names.has(n))) : new Set();
        m.anchor = -1;
        renderList(side);
        updateActionState();
    } catch (e) {
        toast(errText(e), 'error');
        ui[side].pathInput.value = m.path;
    }
}

const refresh = (side) => navigate(side, modelOf(side).path);

function goUp(side) {
    if (side === 'local') { if (state.local.parent) navigate('local', state.local.parent); }
    else navigate('remote', parentRemote(state.remote.path));
}

function childPath(side, name) {
    return side === 'local' ? joinLocal(state.local.path, name) : joinRemote(state.remote.path, name);
}

function renderList(side) {
    const m = modelOf(side);
    const p = ui[side];
    p.pathInput.value = m.path;
    p.upBtn.disabled = side === 'local' ? !m.parent : (m.path === '/' || m.path === '');
    const tbody = clear(p.tbody);
    m.entries.forEach((e, i) => tbody.append(makeRow(side, e, i)));
    p.empty.hidden = m.entries.length > 0;
    p.empty.textContent = 'This folder is empty.';
    const dirs = m.entries.filter((e) => e.isDir).length;
    const bytes = m.entries.reduce((n, e) => n + (e.isDir ? 0 : Math.max(e.size, 0)), 0);
    p.foot.replaceChildren(
        h('span', {}, `${dirs} folder${dirs === 1 ? '' : 's'}, ${m.entries.length - dirs} file${m.entries.length - dirs === 1 ? '' : 's'}`),
        h('span', {}, formatBytes(bytes)));
}

function makeRow(side, e, i) {
    const m = modelOf(side);
    const tr = h('tr', {
        class: `row${e.isDir ? ' dir' : ''}${m.sel.has(e.name) ? ' selected' : ''}`,
        draggable: true,
        title: e.isLink ? `${e.name} (link)` : e.name,
    },
        h('td', {}, h('div', { class: 'name-cell' }, icon(e.isDir ? 'folder' : 'file'), h('span', {}, e.name))),
        h('td', { class: 'size' }, e.isDir ? '' : formatBytes(e.size)),
        h('td', { class: 'date' }, formatDate(e.modTime)),
    );
    tr.addEventListener('click', (ev) => selectRow(side, i, ev));
    tr.addEventListener('dblclick', () => openEntry(side, e));
    tr.addEventListener('dragstart', (ev) => {
        if (!m.sel.has(e.name)) { m.sel = new Set([e.name]); m.anchor = i; paintSelection(side); updateActionState(); }
        dragSide = side;
        ev.dataTransfer.effectAllowed = 'copy';
        ev.dataTransfer.setData('text/plain', [...m.sel].join('\n'));
    });
    tr.addEventListener('dragend', () => { dragSide = null; document.querySelectorAll('.drop-hover,.drop-target').forEach((x) => x.classList.remove('drop-hover', 'drop-target')); });
    if (e.isDir) {
        // dropping on a folder row sends the items into that folder
        tr.addEventListener('dragover', (ev) => {
            if (dragSide && dragSide !== side) { ev.preventDefault(); ev.stopPropagation(); tr.classList.add('drop-target'); }
        });
        tr.addEventListener('dragleave', () => tr.classList.remove('drop-target'));
        tr.addEventListener('drop', (ev) => {
            if (dragSide && dragSide !== side) {
                ev.preventDefault(); ev.stopPropagation();
                tr.classList.remove('drop-target');
                transferSelected(dragSide, childPath(side, e.name));
            }
        });
    }
    return tr;
}

function selectRow(side, i, ev) {
    const m = modelOf(side);
    const name = m.entries[i].name;
    if (ev.shiftKey && m.anchor >= 0) {
        const [a, b] = [Math.min(m.anchor, i), Math.max(m.anchor, i)];
        if (!(ev.ctrlKey || ev.metaKey)) m.sel = new Set();
        for (let k = a; k <= b; k++) m.sel.add(m.entries[k].name);
    } else if (ev.ctrlKey || ev.metaKey) {
        m.sel.has(name) ? m.sel.delete(name) : m.sel.add(name);
        m.anchor = i;
    } else {
        m.sel = new Set([name]);
        m.anchor = i;
    }
    paintSelection(side);
    updateActionState();
}

function paintSelection(side) {
    const m = modelOf(side);
    [...ui[side].tbody.children].forEach((tr, i) => tr.classList.toggle('selected', m.sel.has(m.entries[i].name)));
}

function onListKey(side, e) {
    const m = modelOf(side);
    if (e.key === 'a' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        m.sel = new Set(m.entries.map((x) => x.name));
        paintSelection(side); updateActionState();
    } else if (e.key === 'F5') {
        e.preventDefault(); refresh(side);
    } else if (e.key === 'Backspace' || (e.altKey && e.key === 'ArrowUp')) {
        e.preventDefault(); goUp(side);
    } else if (e.key === 'Delete' && side === 'remote' && state.conn?.caps.modify) {
        deleteSelected();
    } else if (e.key === 'Enter' && m.sel.size === 1) {
        const entry = m.entries.find((x) => x.name === [...m.sel][0]);
        if (entry) openEntry(side, entry);
    }
}

function openEntry(side, entry) {
    if (entry.isDir) navigate(side, childPath(side, entry.name));
    else transferSelected(side, undefined, [entry]);
}

function selectedEntries(side) {
    const m = modelOf(side);
    return m.entries.filter((e) => m.sel.has(e.name));
}

function updateActionState() {
    const c = state.conn;
    const nLocal = state.local.sel.size, nRemote = state.remote.sel.size;
    ui.btnUpload.disabled = !c || nLocal === 0;
    ui.btnDownload.disabled = !c || nRemote === 0;
    ui.tftpUpload.disabled = !c || nLocal === 0;
    if (ui.btnRename) ui.btnRename.disabled = !c?.caps.modify || nRemote !== 1;
    if (ui.btnDelete) ui.btnDelete.disabled = !c?.caps.modify || nRemote === 0;
}

// ---- folder operations ----

async function makeFolder(side) {
    const name = await promptDialog('New folder', 'Folder name', { ok: 'Create' });
    if (!name) return;
    try {
        if (side === 'local') await App.LocalMkdir(state.local.path, name);
        else await App.RemoteMkdir(state.conn.id, joinRemote(state.remote.path, name));
        await refresh(side);
    } catch (e) { toast(errText(e), 'error'); }
}

async function renameSelected() {
    const [entry] = selectedEntries('remote');
    if (!entry) return;
    const name = await promptDialog('Rename', 'New name', { initial: entry.name, ok: 'Rename' });
    if (!name || name === entry.name) return;
    try {
        await App.RemoteRename(state.conn.id, childPath('remote', entry.name), childPath('remote', name));
        await refresh('remote');
    } catch (e) { toast(errText(e), 'error'); }
}

async function deleteSelected() {
    const items = selectedEntries('remote');
    if (!items.length) return;
    const hasDir = items.some((i) => i.isDir);
    const ok = await confirmDialog('Delete from server',
        `Permanently delete ${items.length === 1 ? 'this item' : `these ${items.length} items`}?${hasDir ? ' Folders are deleted with everything inside.' : ''} This cannot be undone.`,
        { ok: 'Delete', danger: true, items: items.map((i) => i.name) });
    if (!ok) return;
    let failed = 0;
    for (const it of items) {
        try { await App.RemoteDelete(state.conn.id, childPath('remote', it.name), it.isDir); }
        catch (e) { failed++; toast(`${it.name}: ${errText(e)}`, 'error'); }
    }
    if (!failed) toast(`Deleted ${items.length} item${items.length === 1 ? '' : 's'}`, 'good');
    await refresh('remote');
}

// ====================================================================
// transfers
// ====================================================================

async function existingNames(side, dir) {
    const m = modelOf(side);
    if (dir === m.path) return new Set(m.entries.map((e) => e.name));
    try {
        const entries = side === 'local' ? (await App.ListLocal(dir)).entries : await App.ListRemote(state.conn.id, dir);
        return new Set((entries || []).map((e) => e.name));
    } catch { return new Set(); } // a folder that does not exist yet cannot conflict
}

async function runTransfer(fn) {
    try { await fn(); } catch (e) { toast(errText(e), 'error'); }
}

// Sends the selection of `from` to the other side. `destOverride` is a folder dropped onto.
async function transferSelected(from, destOverride, only) {
    if (!state.conn) { toast('Connect to a server first'); return; }
    const items = only || selectedEntries(from);
    if (!items.length) return;
    const tftp = !state.conn.caps.browse;

    if (from === 'local') {
        const remoteDir = tftp ? '' : (destOverride ?? state.remote.path);
        if (!tftp && !await confirmOverwrite('remote', remoteDir, items)) return;
        await runTransfer(() => App.Upload(state.conn.id, items.map((e) => joinLocal(state.local.path, e.name)), remoteDir));
    } else {
        const localDir = destOverride ?? state.local.path;
        if (!await confirmOverwrite('local', localDir, items)) return;
        await runTransfer(() => App.Download(state.conn.id,
            items.map((e) => ({ name: e.name, path: joinRemote(state.remote.path, e.name), isDir: e.isDir, size: e.isDir ? -1 : e.size })), localDir));
    }
    toast(`Queued ${items.length} item${items.length === 1 ? '' : 's'}`);
}

async function confirmOverwrite(destSide, dir, items) {
    const existing = await existingNames(destSide, dir);
    const clashes = items.filter((e) => existing.has(e.name)).map((e) => e.name);
    if (!clashes.length) return true;
    return confirmDialog('Replace existing files?',
        `${clashes.length} item${clashes.length === 1 ? ' already exists' : 's already exist'} in the destination and will be overwritten.`,
        { ok: 'Overwrite', danger: true, items: clashes });
}

async function uploadPaths(paths, remoteDir) {
    if (!state.conn) return;
    const dir = state.conn.caps.browse ? remoteDir : '';
    await runTransfer(() => App.Upload(state.conn.id, paths, dir));
    toast(`Queued ${paths.length} item${paths.length === 1 ? '' : 's'}`);
}

// ---- queue ----

function buildQueue() {
    ui.queueList = h('div', { class: 'queue-list' });
    ui.queueCount = h('span', { class: 'muted' });
    ui.clearBtn = button('Clear finished', {
        small: true,
        onClick: async () => {
            const keep = (await App.ClearFinished()) || [];
            state.transfers = new Map([...state.transfers].filter(([id]) => keep.some((k) => k.id === id)));
            renderQueue();
        },
    });
    ui.queue = h('section', { class: 'card queue' },
        h('div', { class: 'queue-head' }, 'Transfers', ui.queueCount, h('span', { class: 'grow' }), ui.clearBtn),
        ui.queueList);
}

let renderPending = false;
const refreshAfter = { upload: debounce(() => state.conn?.caps.browse && refresh('remote'), 400), download: debounce(() => refresh('local'), 400) };

function onTransferEvent(info) {
    const prev = state.transfers.get(info.id);
    state.transfers.set(info.id, info);
    // Coalesce bursts of progress events. setTimeout (not requestAnimationFrame) so the queue
    // stays current even while the window is minimised or covered.
    if (!renderPending) {
        renderPending = true;
        setTimeout(() => { renderPending = false; renderQueue(); }, 60);
    }
    if (prev && prev.state !== info.state && ['done', 'error', 'cancelled'].includes(info.state)) {
        refreshAfter[info.direction]?.();
    }
}

const isActive = (t) => t.state === 'queued' || t.state === 'scanning' || t.state === 'running';

function renderQueue() {
    const all = [...state.transfers.values()];
    const active = all.filter(isActive).length;
    ui.queueCount.textContent = all.length ? `${active} active, ${all.length - active} finished` : '';
    ui.clearBtn.disabled = all.length === active;
    const badge = document.getElementById('client-badge');
    badge.hidden = active === 0;
    badge.textContent = active;
    clear(ui.queueList);
    if (!all.length) {
        ui.queueList.append(h('div', { class: 'empty', style: 'padding:12px' }, 'No transfers yet. Select files and use Upload or Download.'));
        return;
    }
    for (const t of all.slice().reverse()) ui.queueList.append(transferRow(t));
}

const baseName = (p) => (p || '').split(/[\\/]/).filter(Boolean).pop() || p;

function transferRow(t) {
    // Overall figures cover a whole folder; for a single file they equal that file's.
    const total = t.overallTotal, done = t.overallDone;
    const known = total > 0;
    const pct = known ? Math.min(100, Math.round((done / total) * 100)) : (t.state === 'done' ? 100 : 0);
    const indeterminate = t.state === 'scanning' || (t.state === 'running' && !known);

    let sub = '';
    if (t.state === 'queued') sub = 'Waiting…';
    else if (t.state === 'scanning') sub = 'Measuring the folder so the time left can be shown…';
    else if (t.state === 'error') sub = t.error;
    else if (t.state === 'cancelled') sub = 'Cancelled';
    else if (t.state === 'running' && t.filesTotal > 1) {
        // Several files can be moving at once: say how many instead of naming whichever reported last.
        sub = `${t.files} of ${t.filesTotal} files · ` + (t.activeFiles > 1 ? `${t.activeFiles} at once` : baseName(t.file));
    }
    else if (t.state === 'done' && t.files > 1) sub = `${t.files} files`;
    else if (t.file && t.file !== t.name) sub = t.file;
    else if (t.state === 'done') sub = 'Done';

    // Right-hand column: what has moved, how fast, and how long is left (or how long it took).
    let line1 = '', line2 = '';
    if (t.state === 'running') {
        const parts = [];
        if (known) parts.push(`${pct}%`);
        parts.push(known ? `${formatBytes(done)} / ${formatBytes(total)}` : formatBytes(done));
        if (t.speed > 0) parts.push(`${formatBytes(Math.round(t.speed))}/s`);
        line1 = parts.join(' · ');
        if (t.etaSecs === 0) line2 = 'almost done';
        else if (t.etaSecs > 0) line2 = `about ${formatDuration(t.etaSecs)} left`;
        else if (known) line2 = 'estimating time left…';
    } else if (t.state === 'done') {
        const secs = t.startedAt && t.endedAt ? (t.endedAt - t.startedAt) / 1000 : -1;
        line1 = `${formatBytes(total)}${secs >= 0 ? ` in ${formatDuration(secs)}` : ''}`;
        if (t.speed > 0) line2 = `${formatBytes(Math.round(t.speed))}/s average`;
    } else if ((t.state === 'cancelled' || t.state === 'error') && done > 0) {
        line1 = `${formatBytes(done)} sent before it stopped`.replace('sent', t.direction === 'upload' ? 'sent' : 'received');
    }

    return h('div', { class: `xfer ${t.state}` },
        icon(t.direction === 'upload' ? 'upload' : 'download'),
        h('div', { style: 'min-width:0' },
            h('div', { class: 'nm', title: t.name }, t.name),
            h('div', { class: `sub state-${t.state}`, title: sub }, sub)),
        h('div', { class: `bar${indeterminate ? ' indeterminate' : ''}`, role: 'progressbar', 'aria-valuenow': pct, 'aria-valuemin': 0, 'aria-valuemax': 100 },
            h('i', { style: `width:${pct}%` })),
        h('div', { class: 'stat' }, h('div', {}, line1), h('div', { class: 'eta' }, line2)),
        h('div', { class: 'acts' }, isActive(t)
            ? button('Cancel', { small: true, onClick: () => App.CancelTransfer(t.id) }) : null),
    );
}
