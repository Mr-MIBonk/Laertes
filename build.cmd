@echo off
rem Builds laertes.exe next to this file: runs the tests first, then compiles.
rem Double-click it, or run "build.cmd /nopause" from a console or script.
setlocal
cd /d "%~dp0"

where go >nul 2>&1
if errorlevel 1 (
  echo Go is not installed or not on PATH. Get it from https://go.dev/dl/
  goto :fail
)

echo Running tests...
go test ./...
if errorlevel 1 goto :fail

echo Building laertes.exe...
go build -o laertes.exe .
if errorlevel 1 goto :fail

rem Upgrade an installed offline VCI too; Laertes otherwise replaces it only when a session starts.
set "VCI=%ProgramData%\OE\laertes\laertes_vci32.dll"
if exist "%VCI%" (
  copy /y offlinevci\laertes_vci32.dll "%VCI%" >nul 2>&1
  if errorlevel 1 (
    echo Offline VCI not updated: ODIS has it loaded. Close ODIS and build again.
  ) else (
    echo Offline VCI updated: %VCI%
  )
)

echo.
echo Done: %CD%\laertes.exe
if /i not "%~1"=="/nopause" pause
exit /b 0

:fail
echo.
echo Build FAILED.
if /i not "%~1"=="/nopause" pause
exit /b 1
