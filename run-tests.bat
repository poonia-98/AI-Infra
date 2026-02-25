@echo off
REM ─── AI Agent Platform Load Tests ────────────────────────────────────────────
REM Usage: run-tests.bat 192.168.1.x   (pass your machine IP)

set IP=%1
if "%IP%"=="" (
    echo ERROR: Pass your IP as argument
    echo Usage: run-tests.bat 192.168.116.148
    exit /b 1
)

set BASE_URL=http://%IP%:8000/api/v1

echo.
echo ============================================================
echo  TEST 1: Baseline ^(read endpoints, 100 VUs^)
echo ============================================================
docker run --rm -v "%cd%:/src" grafana/k6 run /src/baseline-test.js -e BASE_URL=%BASE_URL%

echo.
echo ============================================================
echo  TEST 2: Lifecycle ^(create/start/stop/delete, 15 VUs^)
echo ============================================================
docker run --rm -v "%cd%:/src" grafana/k6 run /src/lifecycle-test.js -e BASE_URL=%BASE_URL%

echo.
echo Done.