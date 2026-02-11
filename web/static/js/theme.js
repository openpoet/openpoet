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
    },
    monokai: {
        name: 'Monokai',
        metaColor: '#272822',
        preview: ['#272822', '#3e3d32', '#f92672'],
        terminal: {
            background: '#272822',
            foreground: '#f8f8f2',
            cursor: '#f92672',
            selection: 'rgba(249, 38, 114, 0.3)',
            black: '#272822',
            red: '#f92672',
            green: '#a6e22e',
            yellow: '#e6db74',
            blue: '#66d9ef',
            magenta: '#ae81ff',
            cyan: '#a1efe4',
            white: '#f8f8f2',
            brightBlack: '#75715e',
            brightRed: '#f92672',
            brightGreen: '#a6e22e',
            brightYellow: '#e6db74',
            brightBlue: '#66d9ef',
            brightMagenta: '#ae81ff',
            brightCyan: '#a1efe4',
            brightWhite: '#f9f8f5'
        }
    },
    gruvbox: {
        name: 'Gruvbox',
        metaColor: '#282828',
        preview: ['#282828', '#3c3836', '#fe8019'],
        terminal: {
            background: '#282828',
            foreground: '#ebdbb2',
            cursor: '#fe8019',
            selection: 'rgba(254, 128, 25, 0.3)',
            black: '#282828',
            red: '#cc241d',
            green: '#98971a',
            yellow: '#d79921',
            blue: '#458588',
            magenta: '#b16286',
            cyan: '#689d6a',
            white: '#a89984',
            brightBlack: '#928374',
            brightRed: '#fb4934',
            brightGreen: '#b8bb26',
            brightYellow: '#fabd2f',
            brightBlue: '#83a598',
            brightMagenta: '#d3869b',
            brightCyan: '#8ec07c',
            brightWhite: '#ebdbb2'
        }
    },
    tokyonight: {
        name: 'Tokyo Night',
        metaColor: '#1a1b26',
        preview: ['#1a1b26', '#24283b', '#7aa2f7'],
        terminal: {
            background: '#1a1b26',
            foreground: '#c0caf5',
            cursor: '#7aa2f7',
            selection: 'rgba(122, 162, 247, 0.3)',
            black: '#414868',
            red: '#f7768e',
            green: '#9ece6a',
            yellow: '#e0af68',
            blue: '#7aa2f7',
            magenta: '#bb9af7',
            cyan: '#7dcfff',
            white: '#c0caf5',
            brightBlack: '#565f89',
            brightRed: '#f7768e',
            brightGreen: '#9ece6a',
            brightYellow: '#e0af68',
            brightBlue: '#7aa2f7',
            brightMagenta: '#bb9af7',
            brightCyan: '#7dcfff',
            brightWhite: '#c0caf5'
        }
    },
    sakura: {
        name: 'Sakura',
        metaColor: '#fef2f4',
        preview: ['#fef2f4', '#fce4ec', '#d81b60'],
        terminal: {
            background: '#2b1520',
            foreground: '#f3e5f5',
            cursor: '#e91e63',
            selection: 'rgba(233, 30, 99, 0.3)',
            black: '#3e2723',
            red: '#e91e63',
            green: '#66bb6a',
            yellow: '#ffb74d',
            blue: '#7986cb',
            magenta: '#ce93d8',
            cyan: '#4dd0e1',
            white: '#f3e5f5',
            brightBlack: '#6d4c41',
            brightRed: '#f06292',
            brightGreen: '#81c784',
            brightYellow: '#ffd54f',
            brightBlue: '#9fa8da',
            brightMagenta: '#e1bee7',
            brightCyan: '#80deea',
            brightWhite: '#fce4ec'
        }
    },
    cafe: {
        name: 'Cafe',
        metaColor: '#faf6f0',
        preview: ['#faf6f0', '#f0e8da', '#8b5e3c'],
        terminal: {
            background: '#2a1f16',
            foreground: '#ede0d0',
            cursor: '#d4840a',
            selection: 'rgba(212, 132, 10, 0.3)',
            black: '#3e2723',
            red: '#c62828',
            green: '#558b2f',
            yellow: '#f9a825',
            blue: '#6d8aa8',
            magenta: '#8e6a5a',
            cyan: '#00838f',
            white: '#ede0d0',
            brightBlack: '#6d4c41',
            brightRed: '#ef5350',
            brightGreen: '#7cb342',
            brightYellow: '#fdd835',
            brightBlue: '#82a4c0',
            brightMagenta: '#a1887f',
            brightCyan: '#26a69a',
            brightWhite: '#faf6f0'
        }
    },
    arctic: {
        name: 'Arctic',
        metaColor: '#f0f7ff',
        preview: ['#f0f7ff', '#e1eefa', '#0288d1'],
        terminal: {
            background: '#0d1b2a',
            foreground: '#d6e4f0',
            cursor: '#00bcd4',
            selection: 'rgba(0, 188, 212, 0.3)',
            black: '#1b2838',
            red: '#ef5350',
            green: '#26a69a',
            yellow: '#ffca28',
            blue: '#42a5f5',
            magenta: '#7e57c2',
            cyan: '#00bcd4',
            white: '#d6e4f0',
            brightBlack: '#3a5068',
            brightRed: '#e57373',
            brightGreen: '#4db6ac',
            brightYellow: '#ffd54f',
            brightBlue: '#64b5f6',
            brightMagenta: '#9575cd',
            brightCyan: '#26c6da',
            brightWhite: '#eceff1'
        }
    },
    paper: {
        name: 'Paper',
        metaColor: '#f5f0e8',
        preview: ['#f5f0e8', '#ece5d8', '#c41e3a'],
        terminal: {
            background: '#1c1b18',
            foreground: '#ddd8d0',
            cursor: '#c41e3a',
            selection: 'rgba(196, 30, 58, 0.3)',
            black: '#2c2b28',
            red: '#c41e3a',
            green: '#2d6a4f',
            yellow: '#b45309',
            blue: '#3a6ea5',
            magenta: '#7b4b8a',
            cyan: '#1a7d6e',
            white: '#ddd8d0',
            brightBlack: '#5c5b58',
            brightRed: '#e63950',
            brightGreen: '#40916c',
            brightYellow: '#d97706',
            brightBlue: '#5a8ec0',
            brightMagenta: '#9b6da8',
            brightCyan: '#2d9c8a',
            brightWhite: '#f5f0e8'
        }
    },
    brasil: {
        name: 'Brasil',
        metaColor: '#041e42',
        preview: ['#041e42', '#002776', '#009c3b'],
        terminal: {
            background: '#041e42',
            foreground: '#d0e0d8',
            cursor: '#009c3b',
            selection: 'rgba(0, 156, 59, 0.3)',
            black: '#002050',
            red: '#e84040',
            green: '#009c3b',
            yellow: '#ffdf00',
            blue: '#3070c8',
            magenta: '#a060c0',
            cyan: '#28b4a0',
            white: '#d0e0d8',
            brightBlack: '#1a4070',
            brightRed: '#ff6b6b',
            brightGreen: '#30cc50',
            brightYellow: '#ffe84d',
            brightBlue: '#5090e0',
            brightMagenta: '#c080e0',
            brightCyan: '#40d0b8',
            brightWhite: '#e0f0e8'
        }
    }
};

const STORAGE_KEY = 'devmanager-theme';

function getCurrentThemeId() {
    return localStorage.getItem(STORAGE_KEY) || 'light';
}

function getTerminalTheme() {
    const id = getCurrentThemeId();
    return THEMES[id]?.terminal || THEMES.light.terminal;
}

function applyTheme(themeId) {
    if (!THEMES[themeId]) themeId = 'light';

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
