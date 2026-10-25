@echo off
rem Rebuilds the icon of laertes.exe from laertes.svg (the page's favicon): Edge renders the sizes,
rem make_ico.py packs laertes.ico, zig's resource compiler writes ..\rsrc_windows_amd64.syso, which
rem go build links into laertes.exe. The .syso is committed, so building laertes.exe needs none of this.
rem Needs Microsoft Edge, Python and zig (https://ziglang.org).
setlocal
cd /d "%~dp0"
set EDGE=%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe
"%EDGE%" --headless=new --disable-gpu --allow-file-access-from-files --user-data-dir="%TEMP%\laertes-icon-edge" --virtual-time-budget=3000 --dump-dom "file:///%CD:\=/%/render.html" 2>nul | python make_ico.py laertes.ico || exit /b 1
zig rc /:target x86_64 /:output-format coff /fo ..\rsrc_windows_amd64.syso laertes.rc || exit /b 1
echo Done: %CD%\..\rsrc_windows_amd64.syso
