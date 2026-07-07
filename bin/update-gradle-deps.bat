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
REM Temporarily gives a project's builder container egress, refreshes its
REM Gradle dependency cache, and ALWAYS closes the airlock afterwards.
REM
REM Egress + arbitrary build code is the one dangerous combination:
REM `gradlew` executes settings.gradle.kts and every build.gradle.kts during
REM configuration. So before opening the airlock this script REFUSES if the
REM build-affecting set has uncommitted changes — the Gradle build logic AND
REM agent-harness.yaml (the action manifest) — i.e. anything the agent could
REM have edited that a human has not reviewed and committed. This is the
REM same set the scribe's build-logic write gate governs. Override only when
REM you know why, with -force.
REM
REM NOTE: the refresh runs `gradlew` directly via `docker exec`, deliberately
REM bypassing the agent-builder server. So it does NOT stream to the state
REM service's audit/logs; the airlock is a manual human act and THIS script is
REM its record. The builder holds no /state mount either way.
REM
REM Usage:
REM   update-gradle-deps.bat <project> <workspace-path> [egress-network] [-force]
REM   update-gradle-deps.bat myproject c:\projects\myproject
REM ---------------------------------------------------------------

if "%~1"=="" goto :usage
if "%~2"=="" goto :usage

set PROJECT=%~1
set WORKSPACE=%~2
set CONTAINER=%PROJECT%-builder
REM Docker's default `bridge` network always exists and has egress, so it is a
REM reliable temporary airlock without guessing the cell stack's frontend name.
set NETWORK=bridge
set FORCE=0

REM Optional positional [egress-network] and/or -force in any order.
for %%A in (%3 %4) do (
    if /I "%%~A"=="-force" (set FORCE=1) else if not "%%~A"=="" (set NETWORK=%%~A)
)

docker inspect %CONTAINER% >nul 2>&1
if errorlevel 1 (
    echo No such container: %CONTAINER%
    exit /b 1
)

REM ---- Airlock gate: build logic must be clean against committed git state ----
if "%FORCE%"=="1" (
    echo -force: skipping the build-logic review gate.
    goto :open
)

git -C "%WORKSPACE%" rev-parse --is-inside-work-tree >nul 2>&1
if errorlevel 1 (
    echo REFUSING: "%WORKSPACE%" is not a git work tree, so build-logic changes
    echo cannot be reviewed. Re-run with -force only if you trust the tree.
    exit /b 3
)

REM Build-affecting set: the manifest plus all Gradle build logic. `*.gradle.kts`
REM matches root and nested module scripts (settings/build) since a plain git
REM pathspec glob spans directory separators.
set DIRTY=
for /f "delims=" %%L in ('git -C "%WORKSPACE%" status --porcelain -- "agent-harness.yaml" "*.gradle.kts" "gradle/" "buildSrc/" "gradle.properties" "gradlew" "gradlew.bat" 2^>nul') do (
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
REM compile/runtime (JUnit engine + launcher), the JaCoCo agent + report libs
REM for `coverage`, annotation processors, etc. Pure resolution: downloads the
REM JARs, runs no test task and loads no test classes - safer than
REM --test-dry-run and it also reaches coverage's JaCoCo tooling, which a test
REM run alone would miss.
REM
REM The init script is BAKED into the published builder image at
REM /etc/agent-builder/warm-deps.gradle (a platform artifact, not
REM agent-writable). The read-only rootfs deliberately blocks runtime
REM injection (docker cp / writes), so changing it means rebuilding the image.
echo Warming offline dependencies in %CONTAINER% ...
docker exec %CONTAINER% ./gradlew --refresh-dependencies --no-daemon --init-script /etc/agent-builder/warm-deps.gradle build -x test warmAllDeps
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
echo Usage: %~nx0 ^<project^> ^<workspace-path^> [egress-network] [-force]
exit /b 1
