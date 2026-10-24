# Vendor Libraries

Third-party browser libraries served by OpenPoet. They are embedded in the
binary through `web/embed.go` and must stay byte-identical to the published
npm package files (never edit, re-minify or concatenate them). When you
upgrade one, update this table, the matching `LICENSE-*` notice and
`THIRD-PARTY-LICENSES.md` at the repository root in the same commit.

| File | Package | Version | Upstream | License | Notice |
|------|---------|---------|----------|---------|--------|
| `xterm.js` | `@xterm/xterm` (`lib/xterm.js`) | 6.0.0 | https://github.com/xtermjs/xterm.js | MIT (includes VS Code base code, MIT) | `LICENSE-xterm` |
| `xterm.css` | `@xterm/xterm` (`css/xterm.css`) | 6.0.0 | https://github.com/xtermjs/xterm.js | MIT | `LICENSE-xterm` |
| `xterm-addon-fit.js` | `@xterm/addon-fit` (`lib/addon-fit.js`) | 0.11.0 | https://github.com/xtermjs/xterm.js | MIT | `LICENSE-xterm` |
| `xterm-addon-web-links.js` | `@xterm/addon-web-links` (`lib/addon-web-links.js`) | 0.12.0 | https://github.com/xtermjs/xterm.js | MIT | `LICENSE-xterm` |
| `marked.min.js` | `marked` (`marked.min.js`) | 12.0.2 | https://github.com/markedjs/marked | MIT | header in the file |
| `mermaid.min.js` | `mermaid` (`dist/mermaid.min.js`) | 11.12.2 | https://github.com/mermaid-js/mermaid | MIT, bundles permissive third-party code | `LICENSE-mermaid`, trailer in the file |
| `Sortable.min.js` | `sortablejs` (`Sortable.min.js`) | 1.15.6 | https://github.com/SortableJS/Sortable | MIT | header in the file |

`make vendor-js` downloads the xterm.js files above (pinned versions) when
`xterm.js` is missing. The other files are committed as is; to refresh one,
download the exact file from the npm package, for example:

```bash
curl -sL https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js -o web/static/vendor/marked.min.js
curl -sL https://cdn.jsdelivr.net/npm/mermaid@11.12.2/dist/mermaid.min.js -o web/static/vendor/mermaid.min.js
curl -sL https://cdn.jsdelivr.net/npm/sortablejs@1.15.6/Sortable.min.js -o web/static/vendor/Sortable.min.js
```
