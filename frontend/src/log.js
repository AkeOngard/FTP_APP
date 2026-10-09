import * as App from '../wailsjs/go/main/App.js';
import { EventsOn } from '../wailsjs/runtime/runtime.js';
import { h, clear, button, formatClock } from './util.js';

const MAX_LINES = 1000;

let entries = [];
let view, filter, follow;

export async function initLog(root) {
    filter = h('select', { 'aria-label': 'Filter', onchange: render },
        h('option', { value: 'all' }, 'Everything'), h('option', { value: 'error' }, 'Errors only'));
    follow = h('input', { type: 'checkbox', checked: true });
    view = h('div', { class: 'log-view card', role: 'log', 'aria-live': 'off' });

    clear(root).append(
        h('div', { class: 'log-tools' },
            filter,
            h('label', { class: 'check' }, follow, 'Follow new entries'),
            h('span', { class: 'grow' }),
            button('Clear view', { small: true, onClick: () => { entries = []; render(); } })),
        view);

    entries = (await App.GetLogs()) || [];
    render();
    EventsOn('log', (e) => {
        entries.push(e);
        if (entries.length > MAX_LINES) entries.splice(0, entries.length - MAX_LINES);
        if (matches(e)) {
            view.append(line(e));
            while (view.childElementCount > MAX_LINES) view.firstElementChild.remove();
            if (follow.checked) view.scrollTop = view.scrollHeight;
        }
    });
}

const matches = (e) => filter.value === 'all' || e.level === 'error';

function line(e) {
    return h('div', { class: `log-line ${e.level}` },
        h('span', { class: 't' }, formatClock(e.time)),
        h('span', { class: 'svc' }, e.service),
        h('span', { class: 'msg' }, e.message));
}

function render() {
    clear(view);
    const shown = entries.filter(matches);
    if (!shown.length) view.append(h('div', { class: 'empty' }, 'Nothing logged yet.'));
    else view.append(...shown.map(line));
    view.scrollTop = view.scrollHeight;
}
