# Laertes — father of Odis

A web front end for ODIS diagnostics. A small macro running inside ODIS (the bridge) exposes the
unit functions over a local socket; `laertes.exe` serves a web page that uses it to identify units,
read and write coding and adaptations (each write read back and verified), watch measurement values
live, read fault memory with repair guidance, flash firmware or datasets, browse the ODIS database offline and keep a history of
every change. The **Guide** tab in the web page explains setup and use.

![Laertes — Project units with a unit's coding decoded from the ODIS database](docs/screenshot.png)

*Project units with a control unit selected: its coding decoded against the ODIS database into
editable options. (The data shown is illustrative.)*

## Contents

| Path | What it is |
|---|---|
| `laertes.exe` (built) | Web server and UI (Go, single file, SQLite database `laertes.db` next to it) |
| `mcpserver.py` | Bridge macro: TCP server run from the ODIS macro function |
| `mcpserver_handlers.py` | Bridge actions, reloaded by the macro whenever the file changes |
| `index.html` | The web page, embedded into `laertes.exe` |
| `scripts/licenses.sh` | Generates `THIRD_PARTY_LICENSES.txt` for a release |

## Build

Double-click `build.cmd` (needs [Go](https://go.dev/dl/)) — it runs the tests and builds
`laertes.exe`. The two bridge scripts are embedded in it: on every start it copies them to
`C:\ProgramData\OE\macros\` when they are missing or differ, and, if the bridge is not running and ODIS
is not open with its window, starts ODIS in the background as its automation web service and runs
`mcpserver.py` there (`executeMacro`). For use without a VCI it also embeds an offline VCI, a
hardware-less J2534 device built from `offlinevci/offlinevci.c` (the DLL is committed; rebuild it with
`offlinevci\build.cmd`, which needs [zig](https://ziglang.org)).

For development, `laertes.exe -grpstub` also serves a stand-in for VW's online (GRP) sign-in on
`127.0.0.1:7000` (account `admin`/`admin`); ODIS uses it when its `grp.discovery` points there.

## Release

Pushing a tag `vX.Y.Z` runs the release workflow (`.gitea/workflows/release.yml` on Gitea,
`.github/workflows/release.yml` on GitHub): it checks that the tag matches `version`
in `main.go`, runs the tests, builds `laertes.exe` and publishes a release with the executable, the
two bridge scripts, `LICENSE` and `THIRD_PARTY_LICENSES.txt`.

## License

Laertes is released under the [MIT License](LICENSE).

`laertes.exe` includes the Go runtime and these modules, all under permissive licenses (BSD-3-Clause,
MIT or ISC): `github.com/coder/websocket`, `modernc.org/sqlite`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`,
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
