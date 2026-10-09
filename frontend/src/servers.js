import * as App from '../wailsjs/go/main/App.js';
import { EventsOn } from '../wailsjs/runtime/runtime.js';
import {
    h, clear, button, icon, toast, errText, confirmDialog, promptDialog, copyText, debounce,
    formatBytes, formatDuration, formatClock, whoLabel,
} from './util.js';

const SERVICES = [
    { key: 'ftp', title: 'FTP', scheme: 'ftp', blurb: 'Classic file transfer, optional FTPS' },
    { key: 'sftp', title: 'SFTP', scheme: 'sftp', blurb: 'File transfer over SSH' },
    { key: 'tftp', title: 'TFTP', scheme: 'tftp', blurb: 'For devices and network boot' },
];

const ALL_IFACES = '0.0.0.0';
const LOOPBACK = '127.0.0.1';

let root;
let ips = [];
let status = [];
let usersBox;
let activityBox;
const cards = {}; // key -> { setStatus(st), ... }

export async function initServers(el) {
    root = el;
    [ips, status] = await Promise.all([App.LocalIPs(), App.GetStatus()]);
    ips = ips || [];
    const settings = await App.GetSettings();

    const banner = h('div', { id: 'first-run' });
    const grid = h('div', { class: 'servers-grid' }, SERVICES.map((s) => buildCard(s, settings)));
    activityBox = h('section', { class: 'card activity' });
    usersBox = h('div');
    clear(root).append(
        banner, grid,
        h('div', { class: 'section-title' }, 'Server activity'),
        activityBox,
        h('div', { class: 'section-title' }, 'Users'),
        usersBox,
    );
    renderUsers(settings.users || []);
    applyStatus();
    renderActivity(await App.GetServerTransfers());
    EventsOn('srvxfer', renderActivity);

    showInitialPassword(banner);

    const refresh = debounce(refreshStatus, 150);
    EventsOn('log', (e) => {
        if (e.service === 'ftp' || e.service === 'sftp' || e.service === 'tftp') {
            if (/listening|stopped|failed/.test(e.message)) refresh();
        }
    });
    setInterval(refreshStatus, 5000);
}

async function refreshStatus() {
    try {
        status = await App.GetStatus();
        applyStatus();
    } catch { /* the backend is gone; nothing useful to show */ }
}

function applyStatus() {
    for (const st of status) cards[st.name]?.setStatus(st);
}

async function showInitialPassword(banner) {
    const pw = await App.GetInitialPassword();
    if (!pw) return;
    const box = h('div', { class: 'notice info', style: 'margin-bottom:14px' },
        h('div', { class: 'grow' },
            h('strong', {}, 'Welcome. '),
            'An account was created for FTP and SFTP: user ', h('code', { class: 'mono' }, 'admin'),
            ', password ', h('code', { class: 'mono' }, pw),
            '. Write it down; it is not shown again. You can change it under Users.'),
        button('Copy password', { small: true, iconName: 'copy', onClick: () => copyText(pw) }),
        button('Dismiss', { small: true, only: true, iconName: 'x', onClick: () => box.remove() }),
    );
    banner.append(box);
}

// ---------- service cards ----------

function bindOptions(current) {
    const opts = [[LOOPBACK, 'This computer only (127.0.0.1)'], [ALL_IFACES, 'All network interfaces (0.0.0.0)']];
    for (const ip of ips) opts.push([ip, `Only ${ip}`]);
    if (current && !opts.some(([v]) => v === current)) opts.push([current, current]);
    return opts;
}

function field(label, control, hint) {
    return h('label', { class: 'field' }, h('span', {}, label), control, hint ? h('span', { class: 'hint' }, hint) : null);
}

function check(label, checked) {
    const input = h('input', { type: 'checkbox', checked: !!checked });
    return { input, el: h('label', { class: 'check' }, input, label) };
}

function buildCard(svc, settings) {
    const cfg = settings[svc.key];
    const bind = h('select', { value: cfg.bindAddr || ALL_IFACES },
        bindOptions(cfg.bindAddr).map(([v, l]) => h('option', { value: v }, l)));
    const port = h('input', { type: 'number', min: 1, max: 65535, value: cfg.port });
    const rootInput = h('input', { type: 'text', value: cfg.root, spellcheck: 'false' });
    // A relative path is relative to the app's folder; show where it really points.
    const rootHint = h('span', { class: 'hint', style: 'word-break:break-all' });
    const showRoot = async () => {
        const v = rootInput.value.trim();
        rootHint.textContent = v ? `Full path: ${await App.ResolvePath(v)}` : '';
    };
    rootInput.addEventListener('input', debounce(showRoot, 200));
    showRoot();
    const browse = button('Browse…', {
        small: true, iconName: 'folderOpen',
        onClick: async () => {
            const dir = await App.PickFolder('Choose the shared folder');
            if (dir) { rootInput.value = dir; showRoot(); }
        },
    });
    const readOnly = check('Read-only (clients cannot upload, delete or rename)', cfg.readOnly);
    const auto = check('Start automatically when the app opens', cfg.autoStart);

    const extra = {};
    const extraEls = [];
    if (svc.key === 'ftp') {
        extra.pStart = h('input', { type: 'number', min: 0, max: 65535, value: cfg.passiveStart || '' });
        extra.pEnd = h('input', { type: 'number', min: 0, max: 65535, value: cfg.passiveEnd || '' });
        extra.publicHost = h('input', { type: 'text', value: cfg.publicHost || '', placeholder: 'e.g. 203.0.113.7', spellcheck: 'false' });
        extra.anon = check('Allow anonymous login (always read-only)', cfg.allowAnonymous);
        extra.tls = check('Offer FTPS encryption (self-signed certificate)', cfg.tls);
        extra.reqTls = check('Require encryption (refuse plain FTP)', cfg.requireTls);
        extraEls.push(
            h('div', { class: 'row2' },
                field('Passive ports from', extra.pStart),
                field('to', extra.pEnd)),
            field('Public IP (behind NAT)', extra.publicHost, 'Address announced to clients; leave empty on a local network.'),
            h('div', { class: 'checks' }, extra.anon.el, extra.tls.el, extra.reqTls.el),
        );
    }
    const warn = svc.key === 'tftp'
        ? h('div', { class: 'notice warn' }, 'TFTP has no login: anyone who can reach this port can read the shared folder' +
            ' (and write to it unless read-only). Keep it on this computer or a trusted network.')
        : null;

    const pill = h('span', { class: 'pill' }, 'Stopped');
    const toggle = button('Start', { kind: 'primary', iconName: 'play' });
    const urls = h('div', { class: 'urls' });
    const msg = h('span', { class: 'muted' });

    async function save({ quiet = false } = {}) {
        const fresh = await App.GetSettings();
        const c = fresh[svc.key];
        c.bindAddr = bind.value;
        c.port = parseInt(port.value, 10) || 0;
        c.root = rootInput.value.trim();
        c.readOnly = readOnly.input.checked;
        c.autoStart = auto.input.checked;
        if (svc.key === 'ftp') {
            c.passiveStart = parseInt(extra.pStart.value, 10) || 0;
            c.passiveEnd = parseInt(extra.pEnd.value, 10) || 0;
            c.publicHost = extra.publicHost.value.trim();
            c.allowAnonymous = extra.anon.input.checked;
            c.tls = extra.tls.input.checked;
            c.requireTls = extra.reqTls.input.checked;
        }
        await App.SaveService(svc.key, fresh);
        // The backend stores a folder inside the app folder as a relative path; show what was stored.
        rootInput.value = (await App.GetSettings())[svc.key].root;
        showRoot();
        if (!quiet) toast(`${svc.title} settings saved`, 'good');
    }

    const saveBtn = button('Save', {
        onClick: async () => {
            try { await save(); await refreshStatus(); } catch (e) { toast(errText(e), 'error'); }
        },
    });

    toggle.addEventListener('click', async () => {
        toggle.disabled = true;
        try {
            const running = status.find((s) => s.name === svc.key)?.running;
            if (running) {
                await App.StopService(svc.key);
            } else {
                await save({ quiet: true }); // start with what the form shows
                await App.StartService(svc.key);
            }
        } catch (e) {
            toast(`${svc.title}: ${errText(e)}`, 'error');
        } finally {
            toggle.disabled = false;
            await refreshStatus();
        }
    });

    const card = h('section', { class: 'card' },
        h('div', { class: 'svc-head' },
            h('div', { class: 'grow' }, h('div', { class: 'svc-title' }, svc.title)),
            pill, toggle),
        h('div', { class: 'svc-body' },
            h('div', { class: 'svc-blurb' }, svc.blurb),
            warn,
            h('div', { class: 'row2' }, field('Listen on', bind), field('Port', port)),
            h('div', { class: 'field' },
                h('span', {}, 'Shared folder'),
                h('div', { class: 'with-btn' }, rootInput, browse),
                rootHint,
                h('span', { class: 'hint' }, 'A path relative to the app\'s folder (like "share") keeps working when the whole folder is moved or copied.')),
            h('div', { class: 'checks' }, readOnly.el, auto.el),
            extraEls),
        urls,
        h('div', { class: 'svc-foot' }, saveBtn, msg),
    );

    cards[svc.key] = {
        setStatus(st) {
            const sep = st.addr.lastIndexOf(':');
            pill.textContent = st.running ? `Running · port ${st.addr.slice(sep + 1)}` : 'Stopped';
            pill.title = st.running ? st.addr : '';
            pill.classList.toggle('on', st.running);
            toggle.replaceChildren(icon(st.running ? 'stop' : 'play'), document.createTextNode(st.running ? 'Stop' : 'Start'));
            toggle.classList.toggle('primary', !st.running);
            clear(urls);
            if (st.running) {
                const portNum = st.addr.slice(st.addr.lastIndexOf(':') + 1);
                const host = st.addr.slice(0, st.addr.lastIndexOf(':')).replace(/^\[|\]$/g, '');
                const hosts = (host === ALL_IFACES || host === '::') ? ['localhost', ...ips] : [host];
                for (const hst of hosts) {
                    const url = `${svc.scheme}://${hst}:${portNum}`;
                    urls.append(h('button', { class: 'chip', type: 'button', title: 'Copy', onclick: () => copyText(url) }, url));
                }
            }
        },
    };
    return card;
}

// ---------- live server activity ----------

function renderActivity(snap) {
    const active = snap?.active || [];
    const recent = snap?.recent || [];
    clear(activityBox).append(
        h('div', { class: 'activity-head' }, 'Live transfers', h('span', { class: 'grow' }),
            h('span', { class: 'muted', style: 'font-weight:400' }, active.length ? `${active.length} in progress` : '')));
    if (!active.length) {
        activityBox.append(h('div', { class: 'empty', style: 'padding:16px' }, 'Nothing is being transferred right now.'));
    }
    for (const t of active) activityBox.append(activeRow(t));
    if (recent.length) {
        activityBox.append(h('div', { class: 'recent-title' }, 'Recently finished'));
        for (const r of recent) activityBox.append(recentLine(r));
    }
}

function activeRow(t) {
    const known = t.total >= 0;
    const pct = known ? (t.total > 0 ? Math.min(100, Math.round((t.bytes / t.total) * 100)) : 100) : 0;
    const elapsed = Math.max(0, (Date.now() - Date.parse(t.started)) / 1000);

    const parts = [];
    if (known) parts.push(`${pct}%`, `${formatBytes(t.bytes)} / ${formatBytes(t.total)}`);
    else parts.push(`${formatBytes(t.bytes)} ${t.dir === 'upload' ? 'received' : 'sent'}`);
    if (t.speed >= 1) parts.push(`${formatBytes(Math.round(t.speed))}/s`);

    let eta;
    if (t.etaSecs === 0) eta = 'almost done';
    else if (t.etaSecs > 0) eta = `about ${formatDuration(t.etaSecs)} left`;
    else if (known) eta = 'estimating time left…';
    else eta = `running for ${formatDuration(elapsed)} · total size unknown`; // uploads: the client does not announce it

    return h('div', { class: 'xfer running' },
        icon(t.dir === 'upload' ? 'upload' : 'download'),
        h('div', { style: 'min-width:0' },
            h('div', { class: 'nm', title: t.name }, t.name),
            h('div', { class: 'who' }, `${whoLabel(t.user, t.remote)} · ${t.service.toUpperCase()} ${t.dir}`)),
        h('div', { class: `bar${known ? '' : ' indeterminate'}`, role: 'progressbar', 'aria-valuenow': pct, 'aria-valuemin': 0, 'aria-valuemax': 100 },
            h('i', { style: `width:${pct}%` })),
        h('div', { class: 'stat' }, h('div', {}, parts.join(' · ')), h('div', { class: 'eta' }, eta)),
    );
}

function recentLine(r) {
    const who = whoLabel(r.user, r.remote);
    const verb = r.dir === 'upload' ? 'uploaded' : 'downloaded';
    // fewer bytes than the file holds: the client stopped early (or resumed), so do not imply it completed
    const size = r.total >= 0 && r.bytes !== r.total ? `${formatBytes(r.bytes)} of ${formatBytes(r.total)}` : formatBytes(r.bytes);
    const text = r.error
        ? `${who}: ${r.dir} of ${r.name} stopped after ${formatBytes(r.bytes)} in ${formatDuration(r.secs)} (${r.error})`
        : `${who} ${verb} ${r.name}: ${size} in ${formatDuration(r.secs)} (${formatBytes(Math.round(r.speed))}/s)`;
    return h('div', { class: `recent-line${r.error ? ' error' : ''}` },
        icon(r.dir === 'upload' ? 'upload' : 'download'),
        h('span', { class: 'msg', title: text }, text),
        h('span', { class: 't' }, formatClock(r.ended)));
}

// ---------- users ----------

function renderUsers(users) {
    const nameIn = h('input', { type: 'text', placeholder: 'User name', autocomplete: 'off', spellcheck: 'false' });
    const pwIn = h('input', { type: 'password', placeholder: 'Password', autocomplete: 'new-password' });
    const ro = check('Read-only', false);

    async function add() {
        try {
            await App.SetUser(nameIn.value.trim(), pwIn.value, ro.input.checked);
            toast('User saved', 'good');
            await reloadUsers();
        } catch (e) { toast(errText(e), 'error'); }
    }
    pwIn.addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });

    const rows = users.map((u) => {
        const access = h('select', {
            value: u.readOnly ? 'ro' : 'rw', 'aria-label': `Access for ${u.name}`,
            onchange: async () => {
                try { await App.SetUser(u.name, '', access.value === 'ro'); await reloadUsers(); }
                catch (e) { toast(errText(e), 'error'); }
            },
        }, h('option', { value: 'rw' }, 'Read and write'), h('option', { value: 'ro' }, 'Read-only'));
        return h('tr', {},
            h('td', {}, u.name),
            h('td', {}, access),
            h('td', { class: 'actions' },
                button('Change password', {
                    small: true, iconName: 'key',
                    onClick: async () => {
                        const pw = await promptDialog(`Change password for ${u.name}`, 'New password', { password: true, ok: 'Change' });
                        if (!pw) return;
                        try { await App.SetUser(u.name, pw, access.value === 'ro'); toast('Password changed', 'good'); }
                        catch (e) { toast(errText(e), 'error'); }
                    },
                }), ' ',
                button('Delete', {
                    small: true, kind: 'danger', iconName: 'trash',
                    onClick: async () => {
                        if (!await confirmDialog('Delete user', `Delete "${u.name}"? They will no longer be able to sign in.`, { ok: 'Delete', danger: true })) return;
                        try { await App.DeleteUser(u.name); await reloadUsers(); } catch (e) { toast(errText(e), 'error'); }
                    },
                })),
        );
    });

    clear(usersBox).append(h('section', { class: 'card users-card' },
        h('div', { class: 'muted', style: 'padding:6px 14px 8px' }, 'Used by FTP and SFTP. TFTP has no login. Changes apply immediately.'),
        users.length
            ? h('table', { class: 'plain' },
                h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Access'), h('th', {}, ''))),
                h('tbody', {}, rows))
            : h('div', { class: 'empty' }, 'No users yet. FTP and SFTP will refuse every login until you add one.'),
        h('div', { class: 'add-user' }, nameIn, pwIn, ro.el, button('Add user', { kind: 'primary', iconName: 'plus', onClick: add })),
    ));
}

async function reloadUsers() {
    const s = await App.GetSettings();
    renderUsers(s.users || []);
}
