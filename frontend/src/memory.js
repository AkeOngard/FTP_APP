import * as App from '../wailsjs/go/main/App.js';

// Gives memory back to the operating system when nobody is looking at the window. The backend declines
// while a transfer runs. Trimmed pages return on demand, so only what the UI really needs comes back.
const AFTER_START_MS = 5000; // drops what start-up left behind
const AFTER_HIDDEN_MS = 2000; // window minimised or covered
const AFTER_IDLE_MS = 30000; // window open but untouched

let idleTimer = 0;
let hiddenTimer = 0;
let lastInput = 0;

function trim() {
    App.TrimMemory().catch(() => { /* backend gone, nothing to trim */ });
}

function armIdle() {
    clearTimeout(idleTimer);
    idleTimer = setTimeout(trim, AFTER_IDLE_MS);
}

export function initMemory() {
    setTimeout(trim, AFTER_START_MS);
    armIdle();

    // Mouse movement fires constantly; looking at it twice a second is plenty to know the user is there.
    const touch = () => {
        const now = Date.now();
        if (now - lastInput > 500) { lastInput = now; armIdle(); }
    };
    for (const ev of ['pointerdown', 'pointermove', 'keydown', 'wheel']) {
        window.addEventListener(ev, touch, { passive: true });
    }

    document.addEventListener('visibilitychange', () => {
        clearTimeout(hiddenTimer);
        if (document.hidden) hiddenTimer = setTimeout(trim, AFTER_HIDDEN_MS);
        else armIdle();
    });
}
