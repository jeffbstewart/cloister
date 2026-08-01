@echo off
REM Copyright 2026 Jeffrey B. Stewart
REM
REM Licensed under the Apache License, Version 2.0 (the "License");
REM you may not use this file except in compliance with the License.
REM You may obtain a copy of the License at
REM
REM     http://www.apache.org/licenses/LICENSE-2.0
REM
REM Unless required by applicable law or agreed to in writing, software
REM distributed under the License is distributed on an "AS IS" BASIS,
REM WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
REM See the License for the specific language governing permissions and
REM limitations under the License.

setlocal enabledelayedexpansion

REM ---------------------------------------------------------------
REM Dependency airlock (docs/DESIGN.md, "The dependency airlock").
REM Temporarily gives the cell's AGENT container egress, refreshes the
REM Gradle dependency cache in its warmed caches dir (AGENT_CACHES), and
REM ALWAYS closes the airlock afterwards.  Post-grange-cutover shape:
REM the builder is retired; the agent's workbench holds the toolchains,
REM and the checkout lives in the grange at /grange/tree.
REM
REM Egress + arbitrary build code is the one dangerous combination:
REM `gradlew` executes settings.gradle.kts and every build.gradle.kts at
REM configuration time.  Two gates before the airlock opens:
REM
REM   1. NO LIVE AGENT SESSION.  The old airlock opened the builder's
REM      network — a container the model never ran code in directly.
REM      This one opens the agent's own container, so a live session
REM      would hand the model an internet window.  The script refuses
REM      while the workbench tmux session exists; close it first
REM      (`workbench`, then exit the agent).
REM   2. CLEAN BUILD LOGIC.  The grange tree's build-affecting set must
REM      have no uncommitted changes — anything the agent edited that a
REM      human has not reviewed.  Checked via git INSIDE the container
REM      against the checkout's committed state.  Override with -force
REM      only when you know why.
REM
REM The refresh does NOT stream to the state service; the airlock is a
REM manual human act and THIS script is its record.
REM
REM Usage:
REM   update-gradle-deps.bat <project> [egress-network] [-force]
REM   update-gradle-deps.bat myproject
REM ---------------------------------------------------------------

if "%~1"=="" goto :usage

set PROJECT=%~1
set CONTAINER=%PROJECT%-agent
REM Docker's default `bridge` network always exists and has egress, so it is a
REM reliable temporary airlock without guessing the cell stack's names.
set NETWORK=bridge
set FORCE=0

REM Optional positional [egress-network] and/or -force in any order.
for %%A in (%2 %3) do (
    if /I "%%~A"=="-force" (set FORCE=1) else if not "%%~A"=="" (set NETWORK=%%~A)
)

docker inspect %CONTAINER% >nul 2>&1
if errorlevel 1 (
    echo No such container: %CONTAINER%
    exit /b 1
)

REM ---- Gate 0: a provisioned grange (the tree is the gradle project) ----
docker exec %CONTAINER% test -d /grange/tree >nul 2>&1
if errorlevel 1 (
    echo REFUSING: no provisioned workspace at /grange/tree.
    echo Provision the repository first (the archivist's provision verb).
    exit /b 3
)

REM ---- Gate 1: no live agent session while the airlock is open ----
docker exec %CONTAINER% tmux has-session -t agent >nul 2>&1
if not errorlevel 1 (
    echo REFUSING: a live agent session exists in %CONTAINER%.
    echo The airlock gives THIS container internet egress; a running model
    echo must never hold that window.  End the session first:
    echo   docker exec -it %CONTAINER% tmux kill-session -t agent
    exit /b 3
)

REM ---- Gate 2: build logic clean against the checkout's committed state ----
if "%FORCE%"=="1" (
    echo -force: skipping the build-logic review gate.
    goto :open
)

set DIRTY=
for /f "delims=" %%L in ('docker exec -w /grange/tree %CONTAINER% git status --porcelain -- "*.gradle.kts" "gradle/" "buildSrc/" "gradle.properties" "gradlew" "gradlew.bat" 2^>nul') do (
    set DIRTY=1
    echo   changed: %%L
)
if defined DIRTY (
    echo REFUSING to open the airlock: build-affecting files have uncommitted changes.
    echo These files run with, or select, internet-facing build behavior at refresh.
    echo Review and commit them first, or re-run with -force if you trust them.
    exit /b 3
)
echo Build-affecting files are clean against git; opening airlock.

:open
echo Connecting %CONTAINER% to %NETWORK% ...
docker network connect %NETWORK% %CONTAINER%
if errorlevel 1 (
    echo WARNING: connect failed - possibly already connected from an aborted
    echo run. Continuing; the disconnect below closes it either way.
)

REM Warm ALL offline deps in one pass: compile main (build -x test), then the
REM platform init script resolves every resolvable configuration - test
REM compile/runtime, the JaCoCo tooling, annotation processors, etc.  Pure
REM resolution: downloads the JARs, runs no test task.
REM
REM The init script is BAKED into the workbench image at
REM /usr/local/share/cloister/warm-deps.gradle (a platform artifact, not
REM agent-writable; the read-only rootfs blocks runtime injection).  The
REM cache lands in GRADLE_USER_HOME on the AGENT_CACHES bind, shared
REM across the user's projects.
echo Warming offline dependencies in %CONTAINER% ...
docker exec -w /grange/tree %CONTAINER% ./gradlew --refresh-dependencies --no-daemon --init-script /usr/local/share/cloister/warm-deps.gradle build -x test warmAllDeps
set WARMUP_RC=%ERRORLEVEL%

echo Closing airlock: disconnecting %CONTAINER% from %NETWORK% ...
docker network disconnect %NETWORK% %CONTAINER%
if errorlevel 1 (
    echo *** AIRLOCK STILL OPEN: failed to disconnect %CONTAINER% from %NETWORK% ***
    echo *** Close it manually:  docker network disconnect %NETWORK% %CONTAINER% ***
    exit /b 2
)

if not "%WARMUP_RC%"=="0" (
    echo Dependency refresh FAILED with exit %WARMUP_RC%; airlock is closed.
    exit /b %WARMUP_RC%
)

echo.
echo Done: %PROJECT% gradle cache refreshed; airlock closed.
exit /b 0

:usage
echo Usage: %~nx0 ^<project^> [egress-network] [-force]
exit /b 1
