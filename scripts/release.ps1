# Build release binaries for every platform into dist\ (Go runs in Docker).
# Usage (repo root):
#   powershell -ExecutionPolicy Bypass -File scripts\release.ps1 -Version 0.1.0
#   ... -Publish   also create the GitHub release on MorphEdit/conduit (public, binaries only)
param(
    [Parameter(Mandatory)][string]$Version,
    [switch]$Publish,
    [string]$PublicRepo = 'MorphEdit/conduit'
)
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)

if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "version must look like 1.2.3" }
if ((git status --porcelain) -and $Publish) { throw "commit your changes before publishing a release" }
$commit = (git rev-parse --short HEAD).Trim()

Remove-Item -Recurse -Force dist -ErrorAction SilentlyContinue
New-Item -ItemType Directory dist | Out-Null

$targets = 'linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64'
$pkg = 'github.com/conduit-sync/conduit/internal/buildinfo'
$sh = @'
set -e
for t in TARGETS; do
  os=${t%/*}; arch=${t#*/}; ext=''
  if [ "$os" = windows ]; then ext=.exe; fi
  echo "building $os/$arch"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X PKG.Version=VERSION -X PKG.Commit=COMMIT" \
    -o "dist/conduit_${os}_${arch}${ext}" ./cmd/conduit
done
cd dist && sha256sum conduit_* > SHA256SUMS
'@
$sh = $sh.Replace('TARGETS', $targets).Replace('PKG', $pkg).Replace('VERSION', $Version).Replace('COMMIT', $commit)

# Hand the script over as a file: Windows PowerShell mangles quotes in native
# arguments, and the shell in the container wants LF line endings.
[IO.File]::WriteAllText((Join-Path (Get-Location) 'dist\build.sh'), $sh.Replace("`r", ''), [Text.UTF8Encoding]::new($false))
$root = (Get-Location).Path.Replace('\', '/')
docker run --rm -v "${root}:/src" -w /src golang:1.25-alpine sh dist/build.sh
if ($LASTEXITCODE -ne 0) { throw "build failed" }
Remove-Item dist\build.sh
Get-ChildItem dist | Format-Table Name, @{ n = 'MB'; e = { [math]::Round($_.Length / 1MB, 1) } } -AutoSize

if ($Publish) {
    $notes = "Conduit v$Version by MorphEdit.`n`nDownload the binary for your platform below, or use the Dockerfile in the repository. Verify downloads with SHA256SUMS."
    gh release create "v$Version" (Get-ChildItem dist | ForEach-Object FullName) --repo $PublicRepo --title "Conduit v$Version" --notes $notes
    if ($LASTEXITCODE -ne 0) { throw "gh release failed" }
    Write-Host "published https://github.com/$PublicRepo/releases/tag/v$Version"
}
