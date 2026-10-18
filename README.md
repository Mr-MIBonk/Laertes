# Laertes — father of Odis

A web front end for ODIS diagnostics. A small macro running inside ODIS (the bridge) exposes the
unit functions over a local socket; `laertes.exe` serves a web page that uses it to identify units,
read and write coding and adaptations (each write read back and verified), watch measurement values
live, read fault memory with repair guidance, browse the ODIS database offline and keep a history of
every change. The **Guide** tab in the web page explains setup and use.

## Contents

| Path | What it is |
|---|---|
| `laertes.exe` (built) | Web server and UI (Go, single file, SQLite database `laertes.db` next to it) |
| `mcpserver_test.py` | Bridge macro: TCP server run from the ODIS macro function |
| `mcpserver_handlers.py` | Bridge actions, reloaded by the macro whenever the file changes |
| `index.html` | The web page, embedded into `laertes.exe` |
| `scripts/licenses.sh` | Generates `THIRD_PARTY_LICENSES.txt` for a release |

## Build

Double-click `build.cmd` (needs [Go](https://go.dev/dl/)) — it runs the tests and builds
`laertes.exe`. Install the bridge by copying both `mcpserver_*.py` files to `C:\ProgramData\OE\macros\`.

## Release

Pushing a tag `vX.Y.Z` runs `.gitea/workflows/release.yml`: it checks that the tag matches `version`
in `main.go`, runs the tests, builds `laertes.exe` and publishes a release with the executable, the
two bridge scripts, `LICENSE` and `THIRD_PARTY_LICENSES.txt`.

## License

Laertes is released under the [MIT License](LICENSE).

`laertes.exe` includes the Go runtime and these modules, all under permissive licenses (BSD-3-Clause
or MIT): `modernc.org/sqlite`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`,
`golang.org/x/sys`, `github.com/remyoudompheng/bigfft`, `github.com/dustin/go-humanize`,
`github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime`. Their notices are shipped with every
release in `THIRD_PARTY_LICENSES.txt`. The repository contains no third-party data files.

## Disclaimer

- Laertes is an independent project. It is **not affiliated with, endorsed by or supported by**
  Volkswagen AG, its brands, or Ross-Tech LLC. ODIS is a product of Volkswagen AG; VCDS is a product of
  Ross-Tech LLC; all trademarks belong to their owners.
- Laertes contains **no ODIS software, ODIS data or ODX/diagnostic data**. It works only with a copy
  of ODIS you have installed and are licensed to use, through ODIS's own macro function. You are
  responsible for complying with the license terms of your ODIS installation and of any data you use
  (for example label files).
- Coding, adaptation and security-access functions change control units. Use them at your own risk,
  on vehicles or test benches you are authorised to work on. The software is provided "as is", without
  warranty of any kind (see the license).
