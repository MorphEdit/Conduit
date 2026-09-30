# Start the 3-site bench with sample data and open the dashboard.
# Usage (repo root):  powershell -ExecutionPolicy Bypass -File scripts\up.ps1
# Stop:               docker compose --profile branch down -v
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path $PSScriptRoot -Parent)
$OutputEncoding = [Text.UTF8Encoding]::new($false)

function Sql([string]$node, [string]$q) { $q | docker compose --profile branch exec -T "pg-$node" psql -U postgres -d app -q | Out-Null }
function WaitHealthy([int]$port) {
    for ($i = 0; $i -lt 90; $i++) { try { Invoke-RestMethod "http://127.0.0.1:$port/health" -TimeoutSec 2 | Out-Null; return } catch { Start-Sleep 1 } }
    throw "conduit on port $port did not start"
}

Write-Host "starting host + local ..."
$out = docker compose --profile branch up -d --build pg-host pg-local conduit-host conduit-local pg-branch 2>&1
if ($LASTEXITCODE -ne 0) { $out | ForEach-Object { "$_" } | Select-Object -Last 20; throw "docker compose up failed (is Docker Desktop running?)" }
WaitHealthy 7420; WaitHealthy 7421

$hasBranch = (docker compose --profile branch exec -T pg-branch psql -U postgres -d app -tAc "SELECT count(*) FROM pg_namespace WHERE nspname = 'conduit'" 2>$null)
if ("$hasBranch".Trim() -ne '1') {
    Write-Host "branch joins via snapshot ..."
    docker compose --profile branch run --rm --no-deps conduit-branch -config /etc/conduit/conduit.yaml -snapshot-from host 2>&1 | Out-Null
}
docker compose --profile branch up -d conduit-branch 2>&1 | Out-Null
WaitHealthy 7422

if ((docker compose exec -T pg-host psql -U postgres -d app -tAc "SELECT count(*) FROM customers").Trim() -eq '0') {
    Write-Host "adding sample data ..."
    Sql host   "INSERT INTO customers (code, name) VALUES ('C-001', 'บริษัท ก จำกัด'), ('C-002', 'ร้าน ข'); INSERT INTO stock_lots (sku, qty) VALUES ('STEEL-10', 100), ('PIPE-2', 40);"
    Sql local  "INSERT INTO customers (code, name) VALUES ('C-101', 'ลูกค้าจากออฟฟิศ');"
    Sql branch "INSERT INTO customers (code, name) VALUES ('C-201', 'ลูกค้าจากสาขา');"
}

Write-Host ""
Write-Host "dashboard:  http://127.0.0.1:7420/   (host)"
Write-Host "            http://127.0.0.1:7421/   (local)"
Write-Host "            http://127.0.0.1:7422/   (branch)"
Write-Host "try an outage:  docker network disconnect conduit_wan (docker compose ps -q conduit-local)"
Write-Host "reconnect:      docker network connect --alias conduit-local conduit_wan (docker compose ps -q conduit-local)"
Start-Process "http://127.0.0.1:7420/"
