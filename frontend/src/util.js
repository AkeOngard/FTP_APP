// Small DOM helpers. Text from servers (file names, errors) is only ever inserted with
// textContent / createTextNode, never as HTML, so a hostile server cannot inject markup.

export function h(tag, props, ...children) {
    const el = document.createElement(tag);
    const late = [];
    for (const [k, v] of Object.entries(props || {})) {
        if (v == null || v === false) continue;
        if (k === 'class') el.className = v;
        else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
        else if (k === 'value' || k === 'checked') late.push([k, v]); // after <option> children exist
        else if (k === 'disabled' || k === 'hidden' || k === 'draggable') el[k] = v;
        else el.setAttribute(k, v === true ? '' : v);
    }
    for (const c of children.flat(Infinity)) {
        if (c == null || c === false) continue;
        el.append(c.nodeType ? c : document.createTextNode(String(c)));
    }
    for (const [k, v] of late) el[k] = v;
    return el;
}

export function clear(el) { el.replaceChildren(); return el; }

const ICONS = {
    folder: '<path d="M3 6a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
    file: '<path d="M6 3h8l5 5v13H6z"/><path d="M14 3v5h5"/>',
    up: '<path d="M12 19V5"/><path d="m6 11 6-6 6 6"/>',
    refresh: '<path d="M20 11a8 8 0 1 0-2.3 5.7"/><path d="M20 4v7h-7"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    trash: '<path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/>',
    rename: '<path d="M4 20h4L19 9l-4-4L4 16z"/>',
    upload: '<path d="M12 16V4"/><path d="m7 9 5-5 5 5"/><path d="M5 20h14"/>',
    download: '<path d="M12 4v12"/><path d="m7 11 5 5 5-5"/><path d="M5 20h14"/>',
    x: '<path d="M6 6l12 12M18 6 6 18"/>',
    play: '<path d="M7 5v14l12-7z"/>',
    stop: '<rect x="6" y="6" width="12" height="12" rx="1"/>',
    key: '<circle cx="8" cy="15" r="4"/><path d="m11 12 9-9M16 7l3 3"/>',
    plug: '<path d="M9 3v5M15 3v5M6 8h12v4a6 6 0 0 1-12 0zM12 18v3"/>',
    folderOpen: '<path d="M3 8a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v1H3z"/><path d="M3 11h19l-2 8H5z"/>',
    check: '<path d="m5 12 5 5 9-10"/>',
    copy: '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V6a2 2 0 0 1 2-2h9"/>',
};

export function icon(name) {
    // Static, trusted markup only.
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '2');
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('aria-hidden', 'true');
    svg.classList.add('icon-svg');
    svg.innerHTML = ICONS[name] || '';
    return svg;
}

export function button(label, { kind = '', iconName = '', title = '', onClick, small = false, only = false } = {}) {
    const b = h('button', {
        class: ['btn', kind, small ? 'small' : '', only ? 'icon' : ''].filter(Boolean).join(' '),
        type: 'button',
        title: title || (only ? label : ''),
        'aria-label': only ? label : null,
        onclick: onClick,
    });
    if (iconName) b.append(icon(iconName));
    if (!only && label) b.append(document.createTextNode(label));
    return b;
}

export function formatBytes(n) {
    if (n == null || n < 0) return '';
    if (n < 1024) return `${n} B`;
    const units = ['KB', 'MB', 'GB', 'TB'];
    let v = n / 1024, i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

// "3.4s", "45s", "3m 12s", "1h 05m": how long something took or has left.
export function formatDuration(seconds) {
    if (seconds == null || seconds < 0 || !isFinite(seconds)) return '';
    if (seconds < 10) return Number.isInteger(seconds) ? `${seconds}s` : `${seconds.toFixed(1)}s`;
    const s = Math.round(seconds);
    if (s < 60) return `${s}s`;
    if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
    const m = Math.round(s / 60);
    return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}

// "alice@10.0.0.2", or only the address when there is no user (TFTP).
export function whoLabel(user, remote) {
    const host = (remote || '').replace(/:\d+$/, '').replace(/^\[|\]$/g, '');
    if (!host) return user || '';
    return !user || user === '-' ? host : `${user}@${host}`;
}

export function formatDate(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    if (isNaN(d) || d.getFullYear() < 1980) return '';
    const p = (x) => String(x).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function formatClock(iso) {
    const d = new Date(iso);
    const p = (x) => String(x).padStart(2, '0');
    return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function errText(e) {
    if (!e) return 'Unknown error';
    return typeof e === 'string' ? e : (e.message || String(e));
}

// ---- remote (POSIX) path helpers ----
export function joinRemote(dir, name) {
    if (!dir) return name;
    return dir.endsWith('/') ? dir + name : `${dir}/${name}`;
}

export function parentRemote(p) {
    const t = p.replace(/\/+$/, '');
    const i = t.lastIndexOf('/');
    return i <= 0 ? '/' : t.slice(0, i);
}

// ---- local path helper (the separator depends on the OS the backend reports) ----
export function joinLocal(dir, name) {
    const sep = dir.includes('\\') ? '\\' : '/';
    return dir.replace(/[\\/]+$/, '') + sep + name;
}

// ---- toasts ----
export function toast(message, kind = '') {
    const box = document.getElementById('toasts');
    const el = h('div', { class: `toast ${kind}`, role: kind === 'error' ? 'alert' : 'status' }, message);
    box.append(el);
    setTimeout(() => el.remove(), kind === 'error' ? 9000 : 3500);
}

// ---- dialogs ----
function openDialog({ title, body, actions, focus }) {
    return new Promise((resolve) => {
        let done = false;
        // Buttons resolve directly: the dialog's 'close' event can be delayed (or not delivered)
        // while the window is hidden, and the caller must not hang waiting for it.
        const finish = (value) => {
            if (done) return;
            done = true;
            if (dlg.open) dlg.close();
            dlg.remove();
            resolve(value);
        };
        const dlg = h('dialog', {},
            h('div', { class: 'dlg-title' }, title),
            h('div', { class: 'dlg-body' }, body),
            h('div', { class: 'dlg-actions' }, actions.map((a) =>
                button(a.label, { kind: a.kind, onClick: () => finish(a.value) }))),
        );
        dlg.addEventListener('close', () => finish(null)); // Esc
        document.body.append(dlg);
        dlg.showModal();
        (focus ? focus(dlg) : dlg.querySelector('.btn.primary, .btn'))?.focus();
    });
}

export async function confirmDialog(title, message, { ok = 'OK', danger = false, items = [] } = {}) {
    const body = [h('div', {}, message)];
    if (items.length) {
        const shown = items.slice(0, 6).map((i) => h('li', {}, i));
        if (items.length > 6) shown.push(h('li', { class: 'muted' }, `… and ${items.length - 6} more`));
        body.push(h('ul', { class: 'dlg-list' }, shown));
    }
    const r = await openDialog({
        title, body,
        actions: [
            { label: 'Cancel', value: 'no' },
            { label: ok, kind: danger ? 'danger solid' : 'primary', value: 'yes' },
        ],
        focus: (d) => d.querySelector('.dlg-actions .btn:first-child'),
    });
    return r === 'yes';
}

export async function promptDialog(title, label, { initial = '', password = false, ok = 'OK' } = {}) {
    const input = h('input', { type: password ? 'password' : 'text', value: initial, autocomplete: 'off', spellcheck: 'false' });
    const body = [h('label', { class: 'field' }, h('span', {}, label), input)];
    const p = openDialog({
        title, body,
        actions: [{ label: 'Cancel', value: 'no' }, { label: ok, kind: 'primary', value: 'yes' }],
        focus: () => { input.select(); return input; },
    });
    input.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') { e.preventDefault(); input.closest('dialog').querySelector('.btn.primary').click(); }
    });
    const r = await p;
    return r === 'yes' ? input.value : null;
}

export function infoDialog(title, message) {
    return openDialog({ title, body: [h('div', {}, message)], actions: [{ label: 'OK', kind: 'primary', value: 'ok' }] });
}

export function debounce(fn, ms) {
    let t;
    return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}

export async function copyText(text) {
    try {
        await navigator.clipboard.writeText(text);
        toast('Copied', 'good');
    } catch {
        toast('Copy is not available here');
    }
}
