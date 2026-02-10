// Theme System for DevManager
// Manages predefined themes with CSS variable overrides, terminal sync, and persistence

const THEMES = {
    dark: {
        name: 'Midnight',
        metaColor: '#1a1a2e',
        preview: ['#0f0f1a', '#1a1a2e', '#6366f1'],
        terminal: {
            background: '#0f0f1a',
            foreground: '#e4e4e7',
            cursor: '#6366f1',
            selection: 'rgba(99, 102, 241, 0.3)',
            black: '#1a1a2e',
            red: '#ef4444',
            green: '#22c55e',
            yellow: '#f59e0b',
            blue: '#3b82f6',
            magenta: '#a855f7',
            cyan: '#06b6d4',
            white: '#e4e4e7',
            brightBlack: '#3a3a5a',
            brightRed: '#f87171',
            brightGreen: '#4ade80',
            brightYellow: '#fbbf24',
            brightBlue: '#60a5fa',
            brightMagenta: '#c084fc',
            brightCyan: '#22d3ee',
            brightWhite: '#ffffff'
        }
    },
    light: {
        name: 'Daylight',
        metaColor: '#ffffff',
        preview: ['#f5f5f5', '#ffffff', '#4f46e5'],
        terminal: {
            background: '#1e1e2e',
            foreground: '#e4e4e7',
            cursor: '#4f46e5',
            selection: 'rgba(79, 70, 229, 0.3)',
            black: '#282838',
            red: '#ef4444',
            green: '#16a34a',
            yellow: '#d97706',
            blue: '#2563eb',
            magenta: '#9333ea',
            cyan: '#0891b2',
            white: '#e4e4e7',
            brightBlack: '#52525b',
            brightRed: '#f87171',
            brightGreen: '#4ade80',
            brightYellow: '#fbbf24',
            brightBlue: '#60a5fa',
            brightMagenta: '#c084fc',
            brightCyan: '#22d3ee',
            brightWhite: '#ffffff'
        }
    },
    nord: {
        name: 'Nord',
        metaColor: '#3b4252',
        preview: ['#2e3440', '#3b4252', '#88c0d0'],
        terminal: {
            background: '#2e3440',
            foreground: '#eceff4',
            cursor: '#88c0d0',
            selection: 'rgba(136, 192, 208, 0.3)',
            black: '#3b4252',
            red: '#bf616a',
            green: '#a3be8c',
            yellow: '#ebcb8b',
            blue: '#81a1c1',
            magenta: '#b48ead',
            cyan: '#88c0d0',
            white: '#e5e9f0',
            brightBlack: '#4c566a',
            brightRed: '#bf616a',
            brightGreen: '#a3be8c',
            brightYellow: '#ebcb8b',
            brightBlue: '#81a1c1',
            brightMagenta: '#b48ead',
            brightCyan: '#8fbcbb',
            brightWhite: '#eceff4'
        }
    },
    dracula: {
        name: 'Dracula',
        metaColor: '#21222c',
        preview: ['#282a36', '#21222c', '#bd93f9'],
        terminal: {
            background: '#282a36',
            foreground: '#f8f8f2',
            cursor: '#bd93f9',
            selection: 'rgba(189, 147, 249, 0.3)',
            black: '#21222c',
            red: '#ff5555',
            green: '#50fa7b',
            yellow: '#f1fa8c',
            blue: '#6272a4',
            magenta: '#ff79c6',
            cyan: '#8be9fd',
            white: '#f8f8f2',
            brightBlack: '#44475a',
            brightRed: '#ff6e6e',
            brightGreen: '#69ff94',
            brightYellow: '#ffffa5',
            brightBlue: '#d6acff',
            brightMagenta: '#ff92df',
            brightCyan: '#a4ffff',
            brightWhite: '#ffffff'
        }
    },
    solarized: {
        name: 'Solarized',
        metaColor: '#073642',
        preview: ['#002b36', '#073642', '#268bd2'],
        terminal: {
            background: '#002b36',
            foreground: '#93a1a1',
            cursor: '#268bd2',
            selection: 'rgba(38, 139, 210, 0.3)',
            black: '#073642',
            red: '#dc322f',
            green: '#859900',
            yellow: '#b58900',
            blue: '#268bd2',
            magenta: '#d33682',
            cyan: '#2aa198',
            white: '#eee8d5',
            brightBlack: '#586e75',
            brightRed: '#cb4b16',
            brightGreen: '#859900',
            brightYellow: '#b58900',
            brightBlue: '#268bd2',
            brightMagenta: '#6c71c4',
            brightCyan: '#2aa198',
            brightWhite: '#fdf6e3'
        }
    }
};

const STORAGE_KEY = 'devmanager-theme';

function getCurrentThemeId() {
    return localStorage.getItem(STORAGE_KEY) || 'dark';
}

function getTerminalTheme() {
    const id = getCurrentThemeId();
    return THEMES[id]?.terminal || THEMES.dark.terminal;
}

function applyTheme(themeId) {
    if (!THEMES[themeId]) themeId = 'dark';

    // Set data-theme attribute
    document.documentElement.setAttribute('data-theme', themeId);

    // Update meta theme-color
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute('content', THEMES[themeId].metaColor);

    // Sync terminal themes
    if (window.terminalManager && window.terminalManager.terminals) {
        const termTheme = THEMES[themeId].terminal;
        window.terminalManager.terminals.forEach((td) => {
            if (td.terminal) {
                td.terminal.options.theme = termTheme;
            }
        });
    }

    // Persist
    localStorage.setItem(STORAGE_KEY, themeId);

    // Update picker UI if visible
    document.querySelectorAll('.theme-swatch').forEach(el => {
        el.classList.toggle('active', el.dataset.theme === themeId);
    });
}

function initTheme() {
    const id = getCurrentThemeId();
    applyTheme(id);
}

// Expose globally
window.devManagerTheme = {
    THEMES,
    applyTheme,
    getTerminalTheme,
    getCurrentThemeId,
    initTheme
};
