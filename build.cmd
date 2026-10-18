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

echo.
echo Done: %CD%\laertes.exe
if /i not "%~1"=="/nopause" pause
exit /b 0

:fail
echo.
echo Build FAILED.
if /i not "%~1"=="/nopause" pause
exit /b 1
