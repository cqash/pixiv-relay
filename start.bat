@echo off
rem ArkPix Relay Server launcher (Windows)
rem Usage:
rem   start.bat         start server (builds bin\server.exe if missing)
rem   start.bat build   force rebuild, then start
setlocal
cd /d "%~dp0"

set "GO=%CD%\.toolchain\go\bin\go.exe"

if /i "%~1"=="build" goto build
if not exist "bin\server.exe" goto build
goto run

:build
if not exist "%GO%" (
    echo [ERROR] portable Go toolchain not found: %GO%
    echo See AGENTS.md for .toolchain\go setup, or install system Go and edit GO above.
    exit /b 1
)
echo [BUILD] CGO_ENABLED=0 go build -o bin\server.exe ./cmd/server
if not exist bin mkdir bin
set "CGO_ENABLED=0"
"%GO%" build -o bin\server.exe ./cmd/server
if errorlevel 1 (
    echo [ERROR] build failed
    exit /b 1
)

:run
rem Load .env if present (skip comments and blank lines)
if exist ".env" (
    for /f "usebackq eol=# tokens=1,* delims==" %%a in (".env") do (
        if not "%%a"=="" if not "%%b"=="" set "%%a=%%b"
    )
)
if "%PORT%"=="" set "PORT=8080"
echo [START] ArkPix Relay Server, port %PORT%, DATA_DIR=%DATA_DIR%
".\bin\server.exe"
