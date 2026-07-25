$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$dataDir = Join-Path $scriptDir "data"

if (-not (Test-Path $dataDir)) { New-Item -ItemType Directory -Path $dataDir | Out-Null }

# Start embedded PostgreSQL via pg0 (portable data dir)
Start-Process -FilePath "$scriptDir\pg0.exe" -ArgumentList "start --data-dir `"$dataDir\pg0`""

# Wait for pg0 to be ready (up to 30 seconds)
$maxWait = 30; $waited = 0
while ($waited -lt $maxWait) {
    $ready = & "$scriptDir\pg0.exe" psql -c "SELECT 1;" 2>$null
    if ($LASTEXITCODE -eq 0) { break }
    Start-Sleep -Seconds 2; $waited += 2
}
if ($waited -ge $maxWait) { Write-Host "ERROR: pg0 did not start in time"; exit 1 }

# Create database and install pgvector
& "$scriptDir\pg0.exe" psql -c "CREATE DATABASE goclaw;" 2>$null
& "$scriptDir\pg0.exe" psql -d goclaw -c "CREATE EXTENSION IF NOT EXISTS vector;"

# Run migrations
Get-ChildItem "$scriptDir\migrations\*.sql" | ForEach-Object {
    & "$scriptDir\pg0.exe" psql -d goclaw -f $_.FullName
}

# Start GoClaw
& "$scriptDir\goclaw.exe" start
