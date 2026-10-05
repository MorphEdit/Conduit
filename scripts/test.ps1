# Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
# Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
# Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)
# Conduit end-to-end test suite.
# Usage (repo root):  powershell -ExecutionPolicy Bypass -File scripts\test.ps1
#   -Keep   leave the containers running afterwards (for poking at /status)
param([switch]$Keep)

$ErrorActionPreference = 'Continue'
Set-Location (Split-Path $PSScriptRoot -Parent)

# Send SQL on stdin as UTF-8: Windows PowerShell mangles quotes in native args.
$OutputEncoding = [Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)

$tables = @('customers', 'quotations', 'quotation_items', 'stock_lots')
$ports = @{ host = 7420; local = 7421; branch = 7422 }
$adminPw = if ($env:CONDUIT_ADMIN_PASSWORD) { $env:CONDUIT_ADMIN_PASSWORD } else { 'admin' }
$peerPorts = @{ host = 7440; local = 7441; branch = 7442 }
$tmp = Join-Path $env:TEMP 'conduit-test'; New-Item -ItemType Directory -Force $tmp | Out-Null
$script:nodes = @('host', 'local')
$script:passed = 0
$script:failed = 0

function Compose { docker compose --profile branch @args; if ($LASTEXITCODE -ne 0) { throw "docker compose $args failed" } }

function Sql([string]$node, [string]$q) {
    $out = $q | docker compose --profile branch exec -T "pg-$node" psql -U postgres -d app -v ON_ERROR_STOP=1 -qtA -f - 2>&1
    if ($LASTEXITCODE -ne 0) { throw "SQL on $node failed: $($out -join ' ')" }
    return ($out -join "`n").Trim()
}

# Returns $true if the statement was rejected.
function SqlFails([string]$node, [string]$q) {
    try { Sql $node $q | Out-Null; return $false } catch { return $true }
}

function Checksum([string]$node) {
    $parts = foreach ($t in $tables) {
        Sql $node "SELECT '${t}:' || count(*) || ':' || coalesce(md5(string_agg(x::text, '|' ORDER BY x::text)), '-') FROM $t x"
    }
    return ($parts -join ' ')
}

function Pass([string]$m) { Write-Host "  PASS  $m" -ForegroundColor Green; $script:passed++ }
function Fail([string]$m) { Write-Host "  FAIL  $m" -ForegroundColor Red; $script:failed++ }
function Check([bool]$ok, [string]$m) { if ($ok) { Pass $m } else { Fail $m } }
function Section([string]$m) { Write-Host "`n== $m ==" -ForegroundColor Cyan }

function WaitHealthy([string]$node) {
    for ($i = 0; $i -lt 90; $i++) {
        try { Invoke-RestMethod "http://127.0.0.1:$($ports[$node])/health" -TimeoutSec 2 | Out-Null; return } catch { Start-Sleep 1 }
    }
    throw "conduit-$node did not become healthy"
}

function Status([string]$node) { Invoke-RestMethod "http://127.0.0.1:$($ports[$node])/status" -TimeoutSec 5 }

function AssertSynced([string]$name, [int]$timeoutSec = 90) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    do {
        $sums = @{}
        foreach ($n in $script:nodes) { $sums[$n] = Checksum $n }
        $distinct = @($sums.Values | Sort-Object -Unique)
        if ($distinct.Count -eq 1) { Pass "$name  [$($script:nodes -join ', ') identical]"; return }
        Start-Sleep 1
    } while ((Get-Date) -lt $deadline)
    Fail $name
    foreach ($n in $script:nodes) { Write-Host ("        {0,-6} {1}" -f $n, $sums[$n]) }
}

function Container([string]$svc) { (docker compose --profile branch ps -q $svc).Trim() }
function CutWan([string]$node) { docker network disconnect conduit_wan (Container "conduit-$node") | Out-Null }
function HealWan([string]$node) { docker network connect --alias "conduit-$node" conduit_wan (Container "conduit-$node") | Out-Null }

function ConflictCount([string]$node, [string]$kind) {
    [int](Sql $node "SELECT count(*) FROM conduit.conflicts WHERE kind = '$kind'")
}

function WaitPhase([string]$node, [string]$phase, [int]$timeoutSec = 120) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        try { $s = Status $node; if ($s.phase -eq $phase) { return $s } } catch { }
        Start-Sleep 1
    }
    return $null
}

# Call a site's peer port (HTTPS, self-signed: -k) with curl.exe.
# Returns the HTTP status code; the body is left in $script:peerBody.
function PeerCall([string]$node, [string]$method, [string]$path, [string]$auth = '', $body = $null, [switch]$Plain) {
    $scheme = if ($Plain) { 'http' } else { 'https' }
    $out = Join-Path $tmp 'resp.txt'; Remove-Item $out -ErrorAction SilentlyContinue
    $a = @('-sk', '--max-time', '10', '-o', $out, '-w', '%{http_code}', '-X', $method)
    if ($auth) { $a += @('-H', "Authorization: $auth") }
    if ($null -ne $body) {
        $f = Join-Path $tmp 'body.json'
        [IO.File]::WriteAllText($f, ($body | ConvertTo-Json -Depth 10), [Text.UTF8Encoding]::new($false))
        $a += @('-H', 'Content-Type: application/json', '--data-binary', "@$f")
    }
    $code = & curl.exe @a "${scheme}://127.0.0.1:$($peerPorts[$node])$path"
    $script:peerBody = if (Test-Path $out) { Get-Content $out -Raw -Encoding UTF8 } else { '' }
    return [int]$code
}

function Creds([string]$node) { "Conduit " + (Sql $node "SELECT id || ':' || token FROM conduit.node") }

# POST an admin action; returns the HTTP status code.
function Admin([string]$node, [string]$path, [string]$password = $adminPw, $body = $null) {
    $h = @{ 'X-Admin-Password' = $password }
    try {
        $req = @{ Uri = "http://127.0.0.1:$($ports[$node])$path"; Method = 'Post'; Headers = $h; UseBasicParsing = $true }
        if ($body) { $req.Body = ($body | ConvertTo-Json); $req.ContentType = 'application/json' }
        $r = Invoke-WebRequest @req
        $script:lastBody = $r.Content
        return [int]$r.StatusCode
    } catch { return [int]$_.Exception.Response.StatusCode }
}

# ---------------------------------------------------------------------------
Section "0. zero-config setup: found the cluster, join with an invite"
docker compose --profile branch down -v --remove-orphans 2>&1 | Out-Null
$env:LOCAL_INVITE = ''
Compose up -d --build pg-host conduit-host 2>&1 | Out-Null
$hs = WaitPhase host 'running'
Check ($null -ne $hs -and $hs.sequences.offset -eq 1) 'host founded a new cluster (id slot 1)'
Check ((Admin host '/v1/admin/invites' 'wrong') -eq 401) 'invite needs the admin password'
Check ((Admin host '/v1/admin/invites') -eq 200) 'admin created an invite code'
$invite = ($script:lastBody | ConvertFrom-Json).code
$env:LOCAL_INVITE = $invite
Compose up -d pg-local conduit-local 2>&1 | Out-Null
$ls = WaitPhase local 'running'
Check ($null -ne $ls -and $ls.sequences.offset -eq 2) 'local joined with the invite (id slot 2)'
Check ((Sql local "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'") -eq '4') 'empty local database got the table structure copied'
Check ((Sql local "SELECT count(*) FROM pg_trigger WHERE tgname = 'conduit_owner_guard'") -eq '1') 'local adopted the shared owner-only rule for stock_lots'
Start-Sleep 12
$hostPeers = @((Status host).peers | ForEach-Object id)
Check ($hostPeers -contains 'local') 'host started syncing to local by itself (no config edit, no restart)'
$inv = $invite.Substring(5).Replace('-', '+').Replace('_', '/'); while ($inv.Length % 4) { $inv += '=' }
$secret = ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($inv)) | ConvertFrom-Json).s
$code = PeerCall host POST '/v1/join' '' @{ secret = $secret; id = 'intruder'; url = 'https://x:1'; key_hash = 'x'; cert_fp = 'x' }
Check ($code -eq 403) "used invite cannot be used again ($code)"
$fp = (Status host).fingerprint
$inviteFp = ([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($inv)) | ConvertFrom-Json).f
Check ($fp.Length -eq 64 -and $inviteFp -eq $fp) 'invite pins host''s TLS certificate fingerprint'
$memberFp = Sql local "SELECT cert_fp FROM conduit.members WHERE id = 'host'"
Check ($memberFp -eq $fp) 'local learned host''s fingerprint for pinning'

# ---------------------------------------------------------------------------
Section "1. host -> local, every column type"
Sql host @"
BEGIN;
INSERT INTO customers (code, name, email, vip, tags) VALUES
  ('C-A', 'บริษัท ก จำกัด', 'a@example.com', true, '{construction,vip}'),
  ('C-B', 'O''Brien & Sons', NULL, false, NULL),
  ('C-C', 'ร้าน ข', 'b@example.com', false, '{"with space","quote\"d"}');
INSERT INTO quotations (customer_id, doc_no, total, meta, issued_on, attachment, note)
  SELECT id, 'QT-H-0001', 125000.50, '{"vat":7,"items":[1,2]}', '2026-09-30', '\xdeadbeef', E'line1\nline2\ttab'
  FROM customers WHERE code = 'C-A';
INSERT INTO quotation_items SELECT q.id, n, 'งาน ' || n, n * 1.5, 1000 * n FROM quotations q, generate_series(1,3) n;
INSERT INTO stock_lots (sku, qty) VALUES ('STEEL-10', 100), ('PIPE-2', 40);
COMMIT;
UPDATE customers SET vip = true WHERE code = 'C-C';
UPDATE quotation_items SET qty = 99 WHERE line_no = 2;
DELETE FROM customers WHERE code = 'C-B';
"@ | Out-Null
AssertSynced 'basic sync host -> local'

# ---------------------------------------------------------------------------
Section "2. local -> host (two-way) and id ranges"
Sql local "INSERT INTO customers (code, name) VALUES ('C-L1', 'ลูกค้าจาก local'), ('C-L2', 'local two')" | Out-Null
Sql local "INSERT INTO quotations (customer_id, doc_no, total) SELECT id, 'QT-L-0001', 500 FROM customers WHERE code = 'C-L1'" | Out-Null
AssertSynced 'local writes reach host'
$hostIds = Sql host "SELECT string_agg(DISTINCT (id % 10)::text, ',') FROM customers WHERE code LIKE 'C-%' AND code NOT LIKE 'C-L%'"
$localIds = Sql host "SELECT string_agg(DISTINCT (id % 10)::text, ',') FROM customers WHERE code LIKE 'C-L%'"
Check ($hostIds -eq '1' -and $localIds -eq '2') "ids: host-made end in 1, local-made end in 2 (got $hostIds / $localIds)"

# ---------------------------------------------------------------------------
Section "3. no ping-pong"
Start-Sleep 3
$seqBefore = @{ host = Sql host "SELECT last_value FROM conduit.outbox_seq_seq"; local = Sql local "SELECT last_value FROM conduit.outbox_seq_seq" }
Start-Sleep 6
$seqAfter = @{ host = Sql host "SELECT last_value FROM conduit.outbox_seq_seq"; local = Sql local "SELECT last_value FROM conduit.outbox_seq_seq" }
Check ($seqBefore.host -eq $seqAfter.host -and $seqBefore.local -eq $seqAfter.local) "outboxes idle when nobody writes (host $($seqAfter.host), local $($seqAfter.local))"
$hostApplied = Sql host "SELECT applied_seq FROM conduit.inbox_state WHERE origin = 'local'"
Check ($hostApplied -eq $seqAfter.local) "host applied exactly local's $($seqAfter.local) transactions, nothing echoed back"

# ---------------------------------------------------------------------------
Section "4. internet cut: both sides keep inserting"
CutWan local
Sql host  "INSERT INTO customers (name) SELECT 'offline-host-' || g FROM generate_series(1,20) g" | Out-Null
Sql local "INSERT INTO customers (name) SELECT 'offline-local-' || g FROM generate_series(1,20) g" | Out-Null
Start-Sleep 2
$bl = (Status host).peers | Where-Object id -eq 'local'
Check ($bl.backlog -gt 0) "host queues changes for local while cut (backlog $($bl.backlog))"
HealWan local
AssertSynced 'both sides merged after reconnect'
$dupIds = Sql host "SELECT count(*) - count(DISTINCT id) FROM customers"
Check ($dupIds -eq '0') 'no id collisions'

# ---------------------------------------------------------------------------
Section "5. same row edited on both sides while cut (last write wins)"
Sql host "INSERT INTO customers (code, name) VALUES ('C-X', 'original')" | Out-Null
AssertSynced 'row C-X on both'
$before = ConflictCount local 'update_update'
CutWan local
Sql host  "UPDATE customers SET name = 'edited on host (earlier)' WHERE code = 'C-X'" | Out-Null
Start-Sleep 1
Sql local "UPDATE customers SET name = 'edited on local (later)' WHERE code = 'C-X'" | Out-Null
HealWan local
AssertSynced 'converged after concurrent edits'
$final = Sql host "SELECT name FROM customers WHERE code = 'C-X'"
Check ($final -eq 'edited on local (later)') "later edit won everywhere ('$final')"
Check ((ConflictCount local 'update_update') -gt $before) 'conflict recorded on local (older host edit skipped)'

# ---------------------------------------------------------------------------
Section "6. delete vs update while cut"
Sql host "INSERT INTO customers (code, name) VALUES ('C-Y', 'y'), ('C-Z', 'z')" | Out-Null
AssertSynced 'rows C-Y, C-Z on both'
CutWan local
Sql host  "DELETE FROM customers WHERE code = 'C-Y'" | Out-Null          # Y: delete first ...
Sql local "UPDATE customers SET name = 'z edited first' WHERE code = 'C-Z'" | Out-Null  # Z: update first ...
Start-Sleep 1
Sql local "UPDATE customers SET name = 'y edited later' WHERE code = 'C-Y'" | Out-Null  # ... Y: then update
Sql host  "DELETE FROM customers WHERE code = 'C-Z'" | Out-Null          # ... Z: then delete
HealWan local
AssertSynced 'converged after delete/update races'
$y = Sql host "SELECT coalesce((SELECT name FROM customers WHERE code = 'C-Y'), '<gone>')"
$z = Sql host "SELECT coalesce((SELECT name FROM customers WHERE code = 'C-Z'), '<gone>')"
Check ($y -eq 'y edited later') "update after delete wins: C-Y = '$y'"
Check ($z -eq '<gone>') "delete after update wins: C-Z = '$z'"

# ---------------------------------------------------------------------------
Section "7. owner-only table (stock_lots owned by host)"
Check (SqlFails local "INSERT INTO stock_lots (sku, qty) VALUES ('HACK', 1)") 'local cannot write stock_lots'
Check (SqlFails local "UPDATE stock_lots SET qty = 0") 'local cannot update stock_lots'
Sql host "UPDATE stock_lots SET qty = qty - 5 WHERE sku = 'STEEL-10'" | Out-Null
AssertSynced 'host stock change reaches local'

# ---------------------------------------------------------------------------
Section "8. unique conflict (same code created on both sides)"
CutWan local
Sql host  "INSERT INTO customers (code, name) VALUES ('DUP-1', 'dup made on host')" | Out-Null
Sql local "INSERT INTO customers (code, name) VALUES ('DUP-1', 'dup made on local')" | Out-Null
HealWan local
Sql host "INSERT INTO customers (code, name) VALUES ('AFTER-DUP', 'queue still moving')" | Out-Null
$deadline = (Get-Date).AddSeconds(90)
while ((Get-Date) -lt $deadline -and (Sql local "SELECT count(*) FROM customers WHERE code = 'AFTER-DUP'") -ne '1') { Start-Sleep 1 }
Check ((Sql local "SELECT count(*) FROM customers WHERE code = 'AFTER-DUP'") -eq '1') 'queue not blocked by the conflict'
Check ((ConflictCount host 'unique_violation') -ge 1 -and (ConflictCount local 'unique_violation') -ge 1) 'unique_violation recorded on both nodes'
# Manual fix: keep host's row. Delete local's copy, then touch host's row so it is re-sent.
Sql local "DELETE FROM customers WHERE code = 'DUP-1'" | Out-Null
Start-Sleep 2
Sql host "UPDATE customers SET name = name WHERE code = 'DUP-1'" | Out-Null
AssertSynced 'converged after manual resolution'

# ---------------------------------------------------------------------------
Section "9. outages"
Compose stop conduit-local 2>&1 | Out-Null
Sql host "INSERT INTO customers (name) SELECT 'while-conduit-local-down-' || g FROM generate_series(1,30) g" | Out-Null
Compose start conduit-local 2>&1 | Out-Null
AssertSynced 'catch up after conduit-local restart'

Compose stop pg-local 2>&1 | Out-Null
Sql host "UPDATE customers SET email = id || '@x.test' WHERE name LIKE 'while-%'" | Out-Null
Start-Sleep 3
Compose start pg-local 2>&1 | Out-Null
AssertSynced 'catch up after local database restart'

Compose stop conduit-host 2>&1 | Out-Null
Sql host  "INSERT INTO customers (name) SELECT 'while-conduit-host-down-' || g FROM generate_series(1,10) g" | Out-Null
Sql local "INSERT INTO customers (name) SELECT 'local-while-host-conduit-down-' || g FROM generate_series(1,10) g" | Out-Null
Compose start conduit-host 2>&1 | Out-Null
WaitHealthy host
AssertSynced 'catch up after conduit-host restart'

# ---------------------------------------------------------------------------
Section "10. new site with nothing configured: auto Postgres setup, LAN discovery, approval"
Compose up -d pg-branch conduit-branch 2>&1 | Out-Null
$bs = WaitPhase branch 'needs_config'
Check ($null -ne $bs) 'branch waits for permission before touching Postgres settings'
$auto = docker compose --profile branch exec -T pg-branch sh -c 'cat "$PGDATA/postgresql.auto.conf"' | Out-String
Check ($auto -notmatch 'wal_level') 'nothing was changed in Postgres without permission'
Check ((Admin branch '/v1/admin/configure-postgres' 'wrong') -eq 401) 'permission needs the admin password'
Check ((Admin branch '/v1/admin/configure-postgres') -eq 204) 'admin allowed the change'
$bs = WaitPhase branch 'needs_restart'
Check ($null -ne $bs -and $bs.notice -match 'restart') 'branch set wal_level/track_commit_timestamp and asks for one restart'
Compose restart pg-branch 2>&1 | Out-Null
$bs = WaitPhase branch 'waiting_to_join'
$deadline = (Get-Date).AddSeconds(60)
while ((Get-Date) -lt $deadline -and ((Status branch).pairing.status -ne 'requested')) { Start-Sleep 1 }
$pair = (Status branch).pairing
Check ($pair.status -eq 'requested' -and $pair.seed_id -eq 'host') "branch found host on the LAN and asked to join (code $($pair.code))"
$req = (Status host).join_requests | Where-Object node_id -eq 'branch' | Select-Object -First 1
Check ($null -ne $req -and $req.code -eq $pair.code) 'host dashboard shows the same pairing code'
Check ((Admin host "/v1/admin/requests/$($req.id)/approve" 'wrong') -eq 401) 'approval needs the admin password'
Check ((Admin host "/v1/admin/requests/$($req.id)/approve") -eq 204) 'admin approved branch'
$bs = WaitPhase branch 'running'
Check ($null -ne $bs -and $bs.sequences.offset -eq 3) 'branch joined (id slot 3), tables and data copied'
$script:nodes = @('host', 'local', 'branch')
AssertSynced 'branch matches host and local'
Start-Sleep 12
$lp = @((Status local).peers | ForEach-Object id)
Check ($lp -contains 'branch' -and $lp -contains 'host') 'local learned about branch by gossip'
Sql branch "INSERT INTO customers (code, name) VALUES ('C-BR', 'จาก branch')" | Out-Null
Sql local  "INSERT INTO customers (code, name) VALUES ('C-L3', 'local after branch joined')" | Out-Null
Sql host   "UPDATE customers SET vip = true WHERE code = 'C-A'" | Out-Null
AssertSynced 'three-way sync'
$brId = Sql host "SELECT id % 10 FROM customers WHERE code = 'C-BR'"
Check ($brId -eq '3') "branch-made id ends in 3 (got $brId)"
Check (SqlFails branch "DELETE FROM stock_lots") 'branch cannot write stock_lots'
$spurious = Sql branch "SELECT count(*) FROM conduit.conflicts"
Check ($spurious -eq '0') "no spurious conflicts on the new site ($spurious)"

# ---------------------------------------------------------------------------
Section "11. security: TLS + per-site credentials"
$code = PeerCall local GET '/health' -Plain
Check ($code -ne 200) "peer port refuses plain HTTP ($code)"
Check ((PeerCall local GET '/health') -eq 200) 'peer port answers over TLS'
$hostAuth = Creds host
$localAuth = Creds local
$before = [int](Sql local "SELECT applied_seq FROM conduit.inbox_state WHERE origin = 'host'")
$batch = @{ origin = 'host'; txs = @(@{ seq = 1; lsn = '0/0'; commit_time = (Get-Date).ToString('o'); changes = @(
    @{ op = 'I'; s = 'public'; t = 'customers'; new = @(@{ n = 'id'; k = $true; v = '999999' }, @{ n = 'name'; v = 'replayed-must-not-appear' }) }) }) }
$code = PeerCall local POST '/v1/apply' $hostAuth $batch
$ack = $script:peerBody | ConvertFrom-Json
Check ($code -eq 200 -and $ack.applied -eq $before -and (Sql local "SELECT count(*) FROM customers WHERE id = 999999") -eq '0') "replayed old seq is skipped (applied stays $before)"
Check ((PeerCall local POST '/v1/apply' 'Conduit host:wrong-secret' $batch) -eq 401) 'wrong secret rejected'
Check ((PeerCall local POST '/v1/apply' 'Bearer anything' $batch) -eq 401) 'old shared-token style rejected'
Check ((PeerCall host POST '/v1/apply' $localAuth $batch) -eq 403) 'a site cannot send changes pretending to be another site'
Check ((PeerCall local GET '/v1/snapshot') -eq 401) 'snapshot needs credentials'
Check ((PeerCall local GET '/v1/members') -eq 401) 'member list needs credentials'
Check ((PeerCall host GET '/v1/members' $localAuth) -eq 200) 'active member can read the member list'
Check ((Sql host "SELECT count(*) FROM conduit.members WHERE key_hash = '' OR cert_fp = ''") -eq '0') 'every member has its own key hash and certificate pin'

# ---------------------------------------------------------------------------
Section "12. dashboard"
$page = Invoke-WebRequest "http://127.0.0.1:7420/" -UseBasicParsing
Check ($page.StatusCode -eq 200 -and $page.Content -match 'Conduit') 'dashboard served at /'
$mesh = Invoke-RestMethod "http://127.0.0.1:7422/v1/mesh"
$up = @($mesh.nodes | Where-Object reachable).Count
Check ($mesh.nodes.Count -eq 3 -and $up -eq 3) "mesh from branch sees all 3 sites ($up/$($mesh.nodes.Count) reachable)"
CutWan local
$mesh = Invoke-RestMethod "http://127.0.0.1:7420/v1/mesh"
$localNode = $mesh.nodes | Where-Object id -eq 'local'
Check (-not $localNode.reachable) 'mesh marks a cut site unreachable'
HealWan local

# ---------------------------------------------------------------------------
Section "13. remove a site"
$branchAuth = Creds branch
Check ((PeerCall host GET '/v1/members' $branchAuth) -eq 200) 'branch credentials work before removal'
Check ((Admin host '/v1/admin/members/branch/remove') -eq 204) 'admin removed branch on host'
Start-Sleep 15
$hp = @((Status host).peers | ForEach-Object id); $lp = @((Status local).peers | ForEach-Object id)
Check (-not ($hp -contains 'branch') -and -not ($lp -contains 'branch')) 'host and local stopped syncing to branch'
Check ((Status branch).removed -eq $true) 'branch knows it was removed'
Check ((PeerCall host GET '/v1/members' $branchAuth) -eq 403) 'host refuses branch''s credentials right away (and tells it why)'
Check ((PeerCall local GET '/v1/members' $branchAuth) -eq 403) 'local refuses branch''s credentials too (learned by gossip)'
Check ((PeerCall local POST '/v1/apply' $branchAuth @{ origin = 'branch'; txs = @() }) -eq 403) 'removed site can no longer push changes'
Check ((PeerCall local POST '/v1/apply' 'Conduit branch:not-its-secret' @{ origin = 'branch'; txs = @() }) -eq 401) 'someone without branch''s secret just gets 401'
Check ((Sql host "SELECT count(*) FROM conduit.peer_cursor WHERE peer_id = 'branch'") -eq '0') 'branch no longer holds back the outbox'
$script:nodes = @('host', 'local')
Sql host "INSERT INTO customers (code, name) VALUES ('AFTER-REMOVE', 'x')" | Out-Null
AssertSynced 'host and local keep syncing'

# ---------------------------------------------------------------------------
Section "14. apps using an ordinary (non-superuser) database role"
Sql host "CREATE ROLE app_user LOGIN PASSWORD 'x'; GRANT CREATE, USAGE ON SCHEMA public TO app_user;" | Out-Null
Sql local "CREATE ROLE app_user LOGIN PASSWORD 'x'; GRANT USAGE ON SCHEMA public TO app_user; GRANT INSERT ON stock_lots TO app_user;" | Out-Null
$inc = Sql host "SET ROLE app_user; CREATE TABLE app_made (id SERIAL PRIMARY KEY); RESET ROLE; SELECT increment_by FROM pg_sequences WHERE sequencename = 'app_made_id_seq';"
Check ($inc -eq '10') "table created by an app role gets per-site ids at once (increment $inc)"
Check ((Sql host "SET ROLE app_user; SELECT has_table_privilege('conduit.node', 'SELECT')") -match 'f') 'app role cannot read Conduit secrets'
$guard = try { Sql local "SET ROLE app_user; INSERT INTO stock_lots (sku, qty) VALUES ('X', 1);"; '' } catch { "$_" }
Check ($guard -match 'owned by node "host"') 'owner-only guard gives its real message to an app role'

# ---------------------------------------------------------------------------
Write-Host ""
foreach ($n in $script:nodes) {
    $s = Status $n
    $peers = ($s.peers | ForEach-Object { "$($_.id)=acked $($_.acked_seq)" }) -join ', '
    Write-Host ("{0,-6} outbox pending {1,-3} conflicts {2,-3} {3}" -f $n, $s.outbox.pending, $s.conflicts.total, $peers)
}
Write-Host ""
if (-not $Keep) { docker compose --profile branch down -v 2>&1 | Out-Null }
if ($script:failed -eq 0) { Write-Host "ALL $($script:passed) CHECKS PASSED" -ForegroundColor Green; exit 0 }
Write-Host "$($script:failed) FAILED, $($script:passed) passed" -ForegroundColor Red; exit 1
