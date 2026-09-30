# Start the 3-site bench with sample data and open the dashboard.
#   host   founds the cluster
#   local  joins with an invite code
#   branch joins by LAN discovery; this script approves it (pairing codes checked)
# Usage (repo root):  powershell -ExecutionPolicy Bypass -File scripts\up.ps1
# Stop:               docker compose --profile branch down -v
$ErrorActionPreference = 'Continue'
Set-Location (Split-Path $PSScriptRoot -Parent)
$OutputEncoding = [Text.UTF8Encoding]::new($false)
$admin = if ($env:CONDUIT_ADMIN_PASSWORD) { $env:CONDUIT_ADMIN_PASSWORD } else { 'admin' }

function Status([int]$port) { try { Invoke-RestMethod "http://127.0.0.1:$port/status" -TimeoutSec 3 } catch { $null } }
function WaitPhase([int]$port, [string]$phase, [int]$sec = 120) {
    for ($i = 0; $i -lt $sec; $i++) { $s = Status $port; if ($s -and $s.phase -eq $phase) { return $s }; Start-Sleep 1 }
    throw "site on port $port did not reach '$phase'"
}
function Up([string[]]$svcs) {
    $out = docker compose --profile branch up -d --build @svcs 2>&1
    if ($LASTEXITCODE -ne 0) { $out | ForEach-Object { "$_" } | Select-Object -Last 20; throw "docker compose up failed (is Docker Desktop running?)" }
}
function Sql([string]$node, [string]$q) { $q | docker compose --profile branch exec -T "pg-$node" psql -U postgres -d app -q | Out-Null }

Write-Host "host: founding the cluster ..."
Up pg-host, conduit-host
WaitPhase 7420 running | Out-Null

$ls = Status 7421
if (-not $ls -or $ls.phase -ne 'running') {
    Write-Host "local: joining with an invite code ..."
    $env:LOCAL_INVITE = (Invoke-RestMethod http://127.0.0.1:7420/v1/admin/invites -Method Post -Headers @{ 'X-Admin-Password' = $admin }).code
    Up pg-local, conduit-local
    WaitPhase 7421 running | Out-Null
}

$bs = Status 7422
if (-not $bs -or $bs.phase -ne 'running') {
    Write-Host "branch: starting with nothing configured ..."
    Up pg-branch, conduit-branch
    for ($i = 0; $i -lt 60; $i++) { $bs = Status 7422; if ($bs -and $bs.phase -in 'needs_restart', 'waiting_to_join', 'running') { break }; Start-Sleep 1 }
    if ($bs.phase -eq 'needs_restart') {
        Write-Host "branch: Conduit configured Postgres; restarting it once ..."
        docker compose --profile branch restart pg-branch 2>&1 | Out-Null
    }
    if ((Status 7422).phase -ne 'running') {
        Write-Host "branch: waiting for LAN discovery ..."
        for ($i = 0; $i -lt 90; $i++) { $bs = Status 7422; if ($bs.pairing.status -eq 'requested') { break }; Start-Sleep 1 }
        $req = (Status 7420).join_requests | Where-Object code -eq $bs.pairing.code | Select-Object -First 1
        if (-not $req) { throw "branch's join request did not reach host" }
        Write-Host "branch: approving join request (pairing code $($req.code) matches) ..."
        Invoke-RestMethod "http://127.0.0.1:7420/v1/admin/requests/$($req.id)/approve" -Method Post -Headers @{ 'X-Admin-Password' = $admin } | Out-Null
        WaitPhase 7422 running | Out-Null
    }
}

if ((docker compose exec -T pg-host psql -U postgres -d app -tAc "SELECT count(*) FROM customers").Trim() -eq '0') {
    Write-Host "adding sample data ..."
    Sql host  "INSERT INTO customers (code, name) VALUES ('C-001', 'บริษัท ก จำกัด'), ('C-002', 'ร้าน ข'); INSERT INTO stock_lots (sku, qty) VALUES ('STEEL-10', 100), ('PIPE-2', 40);"
    Start-Sleep 3
    Sql local "INSERT INTO customers (code, name) VALUES ('C-101', 'ลูกค้าจากออฟฟิศ');"
    Sql branch "INSERT INTO customers (code, name) VALUES ('C-201', 'ลูกค้าจากสาขา');"
}

Write-Host ""
Write-Host "dashboard:  http://127.0.0.1:7420/   (host)     admin password: $admin"
Write-Host "            http://127.0.0.1:7421/   (local)"
Write-Host "            http://127.0.0.1:7422/   (branch)"
Write-Host "try an outage:  docker network disconnect conduit_wan (docker compose ps -q conduit-local)"
Write-Host "reconnect:      docker network connect --alias conduit-local conduit_wan (docker compose ps -q conduit-local)"
Start-Process "http://127.0.0.1:7420/"
