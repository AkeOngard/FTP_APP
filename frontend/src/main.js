import './style.css';
import * as App from '../wailsjs/go/main/App.js';
import { initServers } from './servers.js';
import { initClient } from './client.js';
import { initLog } from './log.js';
import { initMemory } from './memory.js';
import { toast, errText } from './util.js';

const tabs = [...document.querySelectorAll('.tab')];

function showTab(name) {
    for (const t of tabs) t.setAttribute('aria-selected', String(t.dataset.tab === name));
    for (const v of document.querySelectorAll('.view')) v.hidden = v.id !== `view-${name}`;
    try { sessionStorage.setItem('ftpapp.tab', name); } catch { /* storage unavailable */ }
}

tabs.forEach((t) => t.addEventListener('click', () => showTab(t.dataset.tab)));
tabs.forEach((t, i) => t.addEventListener('keydown', (e) => {
    const next = { ArrowRight: 1, ArrowLeft: -1 }[e.key];
    if (next) { const n = tabs[(i + next + tabs.length) % tabs.length]; n.focus(); showTab(n.dataset.tab); }
}));

async function start() {
    try {
        const saved = sessionStorage.getItem('ftpapp.tab');
        if (saved) showTab(saved);
    } catch { /* ignore */ }

    const info = await App.GetInfo();
    document.getElementById('info').textContent = `v${info.version} · settings in ${info.configDir}`;
    document.getElementById('info').title = info.configDir;

    await Promise.all([
        initServers(document.getElementById('view-servers')),
        initClient(document.getElementById('view-client')),
        initLog(document.getElementById('view-log')),
    ]);
    initMemory();
}

start().catch((e) => toast(`Failed to start: ${errText(e)}`, 'error'));
