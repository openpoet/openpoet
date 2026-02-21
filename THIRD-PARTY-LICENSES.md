# Third-Party Licenses

OpenPoet includes or depends on the following third-party software. Each
package is listed with its license type. Full license texts can be found in
each package's repository.

---

## Go Dependencies (Direct)

| Package | Version | License | Repository |
|---------|---------|---------|------------|
| SherClockHolmes/webpush-go | v1.3.0 | MIT | https://github.com/SherClockHolmes/webpush-go |
| coder/websocket | v1.8.12 | ISC | https://github.com/coder/websocket |
| creack/pty | v1.1.21 | MIT | https://github.com/creack/pty |
| go-chi/chi | v5.1.0 | MIT | https://github.com/go-chi/chi |
| google/uuid | v1.6.0 | BSD-3-Clause | https://github.com/google/uuid |
| jmoiron/sqlx | v1.4.0 | MIT | https://github.com/jmoiron/sqlx |
| pkg/sftp | v1.13.6 | BSD-2-Clause | https://github.com/pkg/sftp |
| sashabaranov/go-openai | v1.29.2 | Apache-2.0 | https://github.com/sashabaranov/go-openai |
| golang.org/x/crypto | v0.28.0 | BSD-3-Clause | https://github.com/golang/crypto |
| modernc.org/sqlite | v1.33.1 | BSD-3-Clause | https://gitlab.com/cznic/sqlite |

## Go Dependencies (Indirect)

| Package | Version | License | Repository |
|---------|---------|---------|------------|
| dustin/go-humanize | v1.0.1 | MIT | https://github.com/dustin/go-humanize |
| golang-jwt/jwt | v3.2.2 | MIT | https://github.com/golang-jwt/jwt |
| hashicorp/golang-lru | v2.0.7 | MPL-2.0 | https://github.com/hashicorp/golang-lru |
| kr/fs | v0.1.0 | BSD-3-Clause | https://github.com/kr/fs |
| mattn/go-isatty | v0.0.20 | MIT | https://github.com/mattn/go-isatty |
| ncruces/go-strftime | v0.1.9 | MIT | https://github.com/ncruces/go-strftime |
| remyoudompheng/bigfft | v0.0.0 | BSD-3-Clause | https://github.com/remyoudompheng/bigfft |
| golang.org/x/sys | v0.26.0 | BSD-3-Clause | https://github.com/golang/sys |
| modernc.org/gc | v3.0.0 | BSD-3-Clause | https://gitlab.com/cznic/gc |
| modernc.org/libc | v1.55.3 | BSD-3-Clause | https://gitlab.com/cznic/libc |
| modernc.org/mathutil | v1.6.0 | BSD-3-Clause | https://gitlab.com/cznic/mathutil |
| modernc.org/memory | v1.8.0 | BSD-3-Clause | https://gitlab.com/cznic/memory |
| modernc.org/strutil | v1.2.0 | BSD-3-Clause | https://gitlab.com/cznic/strutil |
| modernc.org/token | v1.1.0 | BSD-3-Clause | https://gitlab.com/cznic/token |

## Vendored JavaScript Libraries

| Library | Version | License | Repository |
|---------|---------|---------|------------|
| xterm.js | 5.x | MIT | https://github.com/xtermjs/xterm.js |
| xterm-addon-fit | 0.x | MIT | https://github.com/xtermjs/xterm.js |
| xterm-addon-web-links | 0.x | MIT | https://github.com/xtermjs/xterm.js |
| marked | 15.x | MIT | https://github.com/markedjs/marked |

---

## License Summaries

### MIT License
Permits use, modification, distribution, and sublicensing with minimal
restrictions. Requires preservation of copyright and license notices.

### ISC License
Functionally equivalent to MIT. Permits use, modification, and redistribution.
Requires preservation of copyright notice.

### BSD-2-Clause License
Permits use, modification, and redistribution. Requires preservation of
copyright notice and disclaimer.

### BSD-3-Clause License
Same as BSD-2-Clause with an additional clause prohibiting the use of
contributor names for endorsement without permission.

### Apache-2.0 License
Permits use, modification, and distribution. Requires preservation of
copyright notice, license text, and NOTICE file. Provides an express grant of
patent rights from contributors.

### MPL-2.0 License (Mozilla Public License 2.0)
A file-level copyleft license. Modifications to MPL-licensed source files must
remain under MPL-2.0, but MPL-licensed code can be combined with code under
other licenses (including MIT) in a larger work. Using unmodified MPL-2.0
libraries as dependencies does not affect the license of the rest of the
project.

---

## Maintaining This File

When adding or updating dependencies, this file **must** be updated
accordingly. Run `go list -m all` to see the full dependency tree and verify
that any new dependency has a license compatible with MIT.

Licenses that are **compatible** with MIT: MIT, ISC, BSD-2-Clause,
BSD-3-Clause, Apache-2.0, MPL-2.0, Unlicense, CC0.

Licenses that are **NOT compatible** with MIT: GPL-2.0, GPL-3.0, AGPL-3.0,
LGPL-2.1, LGPL-3.0, SSPL, BSL, EUPL (copyleft variants).

If a new dependency uses GPL, AGPL, or any strong copyleft license, it
**cannot** be added to this project without changing the project license.
