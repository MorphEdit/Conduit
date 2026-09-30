# Phase 1 end-to-end test: host -> local one-way sync under failures.
# Usage (from the repo root):  powershell -ExecutionPolicy Bypass -File scripts\demo.ps1 [-Fresh]
param([switch]$Fresh)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)

$tables = @('customers', 'quotations', 'quotation_items')
$script:failed = 0

# Send SQL on stdin as UTF-8: Windows PowerShell mangles quotes in native args.
$OutputEncoding = [Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)

function Sql([string]$svc, [string]$q) {
    $out = $q | docker compose exec -T $svc psql -U postgres -d app -v ON_ERROR_STOP=1 -qtA -f -
    if ($LASTEXITCODE -ne 0) { throw "psql on $svc failed: $q" }
    return ($out -join "`n").Trim()
}

function Checksum([string]$svc) {
    $parts = foreach ($t in $tables) {
        Sql $svc "SELECT '${t}:' || count(*) || ':' || coalesce(md5(string_agg(x::text, '|' ORDER BY x::text)), '-') FROM $t x"
    }
    return ($parts -join ' ')
}

function Status([int]$port) {
    try { return Invoke-RestMethod "http://127.0.0.1:$port/status" -TimeoutSec 3 } catch { return $null }
}

function WaitHealthy([int]$port) {
    for ($i = 0; $i -lt 60; $i++) {
        try { Invoke-RestMethod "http://127.0.0.1:$port/health" -TimeoutSec 2 | Out-Null; return } catch { Start-Sleep 1 }
    }
    throw "conduit on port $port did not become healthy"
}

function AssertSynced([string]$name, [int]$timeoutSec = 30) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    do {
        $h = Checksum 'pg-host'; $l = Checksum 'pg-local'
        if ($h -eq $l) { Write-Host "  PASS  $name" -ForegroundColor Green; Write-Host "        $h"; return }
        Start-Sleep 1
    } while ((Get-Date) -lt $deadline)
    Write-Host "  FAIL  $name" -ForegroundColor Red
    Write-Host "        host : $h"; Write-Host "        local: $l"
    $script:failed++
}

function Backlog { $s = Status 7420; if ($s) { return $s.peers[0].backlog } else { return '?' } }

Write-Host "== starting test bench ==" -ForegroundColor Cyan
if ($Fresh) { docker compose down -v | Out-Null }
docker compose up -d --build
if ($LASTEXITCODE -ne 0) { throw "docker compose up failed" }
WaitHealthy 7420; WaitHealthy 7421

Write-Host "`n== 1. basic insert / update / delete, all column types ==" -ForegroundColor Cyan
Sql 'pg-host' @"
BEGIN;
INSERT INTO customers (name, email, vip, tags) VALUES
  ('บริษัท ก จำกัด', 'a@example.com', true, '{construction,vip}'),
  ('O''Brien & Sons', NULL, false, NULL),
  ('ร้าน ข', 'b@example.com', false, '{"with space","quote\"d"}');
INSERT INTO quotations (customer_id, doc_no, total, meta, issued_on, attachment, note)
  SELECT id, 'QT-2026-0001', 125000.50, '{"vat":7,"items":[1,2]}', '2026-09-30', '\xdeadbeef', E'line1\nline2\ttab'
  FROM customers WHERE email = 'a@example.com';
INSERT INTO quotation_items SELECT q.id, n, 'งาน ' || n, n * 1.5, 1000 * n FROM quotations q, generate_series(1,3) n;
COMMIT;
UPDATE customers SET vip = true, updated_at = now() WHERE name = 'ร้าน ข';
UPDATE quotation_items SET qty = 99 WHERE line_no = 2;
DELETE FROM customers WHERE name = 'O''Brien & Sons';
"@ | Out-Null
AssertSynced 'basic sync'

Write-Host "`n== 2. conduit-local down while host keeps writing ==" -ForegroundColor Cyan
docker compose stop conduit-local | Out-Null
Sql 'pg-host' "INSERT INTO customers (name) SELECT 'offline-' || g FROM generate_series(1,50) g" | Out-Null
Start-Sleep 3
Write-Host "        backlog while local is down: $(Backlog)"
docker compose start conduit-local | Out-Null
AssertSynced 'catch up after conduit-local restart'

Write-Host "`n== 3. pg-local (database) down ==" -ForegroundColor Cyan
docker compose stop pg-local | Out-Null
Sql 'pg-host' "UPDATE customers SET email = name || '@x.test' WHERE name LIKE 'offline-%'" | Out-Null
Sql 'pg-host' "DELETE FROM customers WHERE name IN ('offline-1','offline-2')" | Out-Null
Start-Sleep 3
Write-Host "        backlog while pg-local is down: $(Backlog)"
docker compose start pg-local | Out-Null
AssertSynced 'catch up after pg-local restart' 60

Write-Host "`n== 4. conduit-host down (changes wait in the replication slot) ==" -ForegroundColor Cyan
docker compose stop conduit-host | Out-Null
Sql 'pg-host' "INSERT INTO customers (name) SELECT 'while-host-conduit-down-' || g FROM generate_series(1,20) g" | Out-Null
docker compose start conduit-host | Out-Null
WaitHealthy 7420
AssertSynced 'catch up after conduit-host restart'

Write-Host "`n== 5. redelivery is harmless (idempotent) ==" -ForegroundColor Cyan
# Replay an old seq straight at conduit-local, as a sender would after a lost ack.
$token = if ($env:CONDUIT_TOKEN) { $env:CONDUIT_TOKEN } else { 'dev-secret-change-me' }
$before = [int](Sql 'pg-local' "SELECT applied_seq FROM conduit.inbox_state WHERE origin = 'host'")
$batch = @{ origin = 'host'; txs = @(@{ seq = 1; lsn = '0/0'; commit_time = (Get-Date).ToString('o'); changes = @(
    @{ op = 'I'; s = 'public'; t = 'customers'; new = @(@{ n = 'id'; k = $true; v = '999999' }, @{ n = 'name'; v = 'replayed-must-not-appear' }) }) }) }
$ack = Invoke-RestMethod 'http://127.0.0.1:7421/v1/apply' -Method Post -ContentType 'application/json' `
    -Headers @{ Authorization = "Bearer $token" } -Body ($batch | ConvertTo-Json -Depth 10)
$ghost = Sql 'pg-local' "SELECT count(*) FROM customers WHERE id = 999999"
if ($ack.applied -eq $before -and $ghost -eq '0') { Write-Host "  PASS  replayed seq 1 skipped (applied stays $before)" -ForegroundColor Green }
else { Write-Host "  FAIL  replay: ack=$($ack | ConvertTo-Json -Compress) ghost rows=$ghost" -ForegroundColor Red; $script:failed++ }

$bad = try { Invoke-WebRequest 'http://127.0.0.1:7421/v1/apply' -Method Post -Body '{}' -Headers @{ Authorization = 'Bearer wrong' } -UseBasicParsing; 200 } catch { [int]$_.Exception.Response.StatusCode }
if ($bad -eq 401) { Write-Host "  PASS  wrong token rejected (401)" -ForegroundColor Green }
else { Write-Host "  FAIL  wrong token got $bad" -ForegroundColor Red; $script:failed++ }

Write-Host ""
$s = Status 7420
Write-Host "host status: outbox pending=$($s.outbox.pending) max_seq=$($s.outbox.max_seq) peer acked=$($s.peers[0].acked_seq)"
if ($script:failed -eq 0) { Write-Host "ALL PASSED" -ForegroundColor Green; exit 0 }
Write-Host "$script:failed FAILED" -ForegroundColor Red; exit 1
