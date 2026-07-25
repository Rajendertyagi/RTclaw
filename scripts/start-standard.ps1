$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$dataDir = Join-Path $scriptDir "data"

if (-not (Test-Path $dataDir)) { New-Item -ItemType Directory -Path $dataDir | Out-Null }

# Start embedded PostgreSQL via pg0 (portable data dir)
Start-Process -FilePath "$scriptDir\pg0.exe" -ArgumentList "start --port 5433 --data-dir `"$dataDir\pg0`""
Start-Sleep -Seconds 3

# Create database and install pgvector
& "$scriptDir\pg0.exe" psql -c "CREATE DATABASE goclaw;" 2>$null
& "$scriptDir\pg0.exe" psql -c "CREATE EXTENSION IF NOT EXISTS vector;"

# Run migrations
Get-ChildItem "$scriptDir\migrations\*.sql" | ForEach-Object {
    & "$scriptDir\pg0.exe" psql -f $_.FullName
}

# Start GoClaw
& "$scriptDir\goclaw.exe" start
