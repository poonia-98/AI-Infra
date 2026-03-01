# Get all directories that contain a go.mod file
$goDirs = Get-ChildItem -Directory | Where-Object { Test-Path "$($_.FullName)\go.mod" } | Select-Object -ExpandProperty Name

foreach ($svc in $goDirs) {
    Write-Host "`nChecking $svc ..." -ForegroundColor Cyan
    Push-Location $svc
    go mod download
    go build -o nul .
    if ($LASTEXITCODE -eq 0) {
        Write-Host "$svc OK" -ForegroundColor Green
    } else {
        Write-Host "$svc FAILED" -ForegroundColor Red
        exit 1
    }
    Pop-Location
}
Write-Host "`nAll Go services built successfully!" -ForegroundColor Green