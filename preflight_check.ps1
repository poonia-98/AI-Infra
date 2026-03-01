<#
.SYNOPSIS
    Pre-flight check for dependency and build issues across Go, Python, and Docker.
.DESCRIPTION
    - Verifies all Go modules compile, checksums match, and dependencies are tidy.
    - Dry‑runs Python requirements installation to catch missing/incompatible packages.
    - Lints Dockerfiles (warnings only – does not fail the build).
.NOTES
    Run this script from the repository root (platform\) before building with Docker.
#>

$ErrorActionPreference = "Stop"

# -----------------------------------------------------------------------------
# Helper: Write coloured output
# -----------------------------------------------------------------------------
function Write-Info   { Write-Host "[INFO] $args" -ForegroundColor Cyan }
function Write-Success{ Write-Host "[OK]   $args" -ForegroundColor Green }
function Write-Warn   { Write-Host "[WARN] $args" -ForegroundColor Yellow }
function Write-Error  { Write-Host "[ERROR] $args" -ForegroundColor Red; $global:Failed = $true }

# -----------------------------------------------------------------------------
# Check Go services
# -----------------------------------------------------------------------------
function Check-GoService {
    param([string]$Path)
    Push-Location $Path
    Write-Info "Checking Go service: $Path"

    # 1. Download modules
    go mod download
    if ($LASTEXITCODE -ne 0) { Write-Error "go mod download failed in $Path"; Pop-Location; return }

    # 2. Verify checksums (detects tampered dependencies)
    go mod verify
    if ($LASTEXITCODE -ne 0) { Write-Error "go mod verify failed in $Path"; Pop-Location; return }

    # 3. Check if go.mod needs tidying (dry run: capture what 'go mod tidy' would change)
    $beforeMod = Get-Content go.mod -Raw
    $beforeSum = if (Test-Path go.sum) { Get-Content go.sum -Raw } else { "" }
    go mod tidy -v
    $afterMod = Get-Content go.mod -Raw
    $afterSum = if (Test-Path go.sum) { Get-Content go.sum -Raw } else { "" }

    if ($beforeMod -ne $afterMod -or $beforeSum -ne $afterSum) {
        Write-Warn "go.mod or go.sum changed after 'go mod tidy'. Run 'go mod tidy' manually to clean up."
    }

    # 4. Compile (fast, no binary)
    go build -o nul .
    if ($LASTEXITCODE -eq 0) {
        Write-Success "$Path compiled"
    } else {
        Write-Error "$Path compilation failed"
    }

    Pop-Location
}

# -----------------------------------------------------------------------------
# Check Python services (requirements.txt)
# -----------------------------------------------------------------------------
function Check-PythonService {
    param([string]$Path, [string]$ReqFile = "requirements.txt")

    Push-Location $Path
    Write-Info "Checking Python service: $Path"

    if (-not (Test-Path $ReqFile)) {
        Write-Warn "No $ReqFile found, skipping"
        Pop-Location
        return
    }

    # Check if pip is available
    $pip = Get-Command pip -ErrorAction SilentlyContinue
    if (-not $pip) {
        Write-Warn "pip not found in PATH, skipping Python dependency check"
        Pop-Location
        return
    }

    # Dry‑run installation – reports conflicts/missing packages
    $output = pip install --dry-run -r $ReqFile 2>&1 | Out-String
    if ($LASTEXITCODE -eq 0) {
        Write-Success "$ReqFile is installable"
    } else {
        Write-Error "pip dry-run failed for $Path`n$output"
    }

    Pop-Location
}

# -----------------------------------------------------------------------------
# Lint Dockerfiles using hadolint (warnings only – does not fail the build)
# -----------------------------------------------------------------------------
function Check-Dockerfile {
    param([string]$Path)

    $df = Join-Path $Path "Dockerfile"
    if (-not (Test-Path $df)) { return }

    Write-Info "Linting Dockerfile: $df"

    # Check if Docker is available
    $docker = Get-Command docker -ErrorAction SilentlyContinue
    if (-not $docker) {
        Write-Warn "docker not found, skipping hadolint"
        return
    }

    # Run hadolint by piping the Dockerfile content
    Get-Content $df | docker run --rm -i hadolint/hadolint 2>&1 | ForEach-Object {
        Write-Warn "hadolint: $_"
    }
    # Note: We do NOT set $global:Failed based on hadolint warnings.
}

# -----------------------------------------------------------------------------
# Main execution
# -----------------------------------------------------------------------------
$global:Failed = $false

# Find all Go services (directories with go.mod)
$goDirs = Get-ChildItem -Directory | Where-Object { Test-Path "$($_.FullName)\go.mod" } | Select-Object -ExpandProperty Name
foreach ($dir in $goDirs) {
    Check-GoService -Path $dir
}

# Find Python services (directories with requirements.txt) – you can add more patterns
$pythonDirs = Get-ChildItem -Directory | Where-Object { Test-Path "$($_.FullName)\requirements.txt" } | Select-Object -ExpandProperty Name
foreach ($dir in $pythonDirs) {
    Check-PythonService -Path $dir
}

# Also check the sdk/python directory separately (if exists)
if (Test-Path "sdk\python\requirements.txt") {
    Check-PythonService -Path "sdk\python" -ReqFile "requirements.txt"
}

# Lint all Dockerfiles in the repository (including subdirectories) – warnings only
$dockerfiles = Get-ChildItem -Recurse -Filter "Dockerfile" | ForEach-Object { $_.DirectoryName }
foreach ($dir in $dockerfiles | Sort-Object -Unique) {
    Check-Dockerfile -Path $dir
}

# Final summary
if ($global:Failed) {
    Write-Host "`n❌ Pre‑flight checks failed. Please fix the errors above before building." -ForegroundColor Red
    exit 1
} else {
    Write-Host "`n✅ All critical checks passed. Dockerfile warnings (if any) are non‑fatal. You can safely run your Docker build." -ForegroundColor Green
}