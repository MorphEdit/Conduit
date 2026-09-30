# Copy public\ (docs, examples, license, templates) into a clone of the
# public repository MorphEdit/conduit, then commit and push it.
# The public repository never receives source code.
# Usage (repo root):
#   powershell -ExecutionPolicy Bypass -File scripts\sync-public.ps1 -Message "Update docs"
param(
    [string]$Message = 'Update docs',
    [string]$Clone = (Join-Path (Split-Path (Split-Path $PSScriptRoot -Parent) -Parent) 'conduit-public'),
    [string]$PublicRepo = 'MorphEdit/conduit'
)
$ErrorActionPreference = 'Stop'
$src = Join-Path (Split-Path $PSScriptRoot -Parent) 'public'

if (-not (Test-Path (Join-Path $Clone '.git'))) {
    gh repo clone $PublicRepo $Clone
    if ($LASTEXITCODE -ne 0) { throw "could not clone $PublicRepo" }
}
# Mirror: remove everything except .git, then copy public\ over.
Get-ChildItem $Clone -Force | Where-Object Name -ne '.git' | Remove-Item -Recurse -Force
Copy-Item (Join-Path $src '*') $Clone -Recurse -Force
Copy-Item (Join-Path $src '.github') $Clone -Recurse -Force

$bad = Get-ChildItem $Clone -Recurse -File -Include *.go, go.mod, go.sum | Where-Object FullName -notmatch '\\.git\\'
if ($bad) { throw "refusing to publish source files: $($bad.FullName -join ', ')" }

Push-Location $Clone
try {
    git add -A
    if (git status --porcelain) {
        git commit -m $Message
        git push -u origin HEAD:main
    } else {
        Write-Host 'public repository already up to date'
    }
} finally { Pop-Location }
