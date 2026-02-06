# Vendor Libraries

This directory should contain the following vendor libraries:

## xterm.js
Download from: https://www.npmjs.com/package/xterm

Required files:
- xterm.js (or xterm.min.js)
- xterm.css
- xterm-addon-fit.js
- xterm-addon-web-links.js

You can install via npm and copy the files:
```bash
npm install xterm xterm-addon-fit xterm-addon-web-links
cp node_modules/xterm/lib/xterm.js web/static/vendor/
cp node_modules/xterm/css/xterm.css web/static/vendor/
cp node_modules/xterm-addon-fit/lib/xterm-addon-fit.js web/static/vendor/
cp node_modules/xterm-addon-web-links/lib/xterm-addon-web-links.js web/static/vendor/
```

Or use CDN links in the HTML:
```html
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/xterm@5/css/xterm.css">
<script src="https://cdn.jsdelivr.net/npm/xterm@5/lib/xterm.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0/lib/xterm-addon-fit.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/xterm-addon-web-links@0/lib/xterm-addon-web-links.min.js"></script>
```
