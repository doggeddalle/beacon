<#
.SYNOPSIS
    Build the Beacon ASUSTOR App Central package (.apkg) for arm64.

.DESCRIPTION
    Cross-compiles the linux/arm64 binary, stages the package tree (CONTROL
    metadata + payload), generates the icon, and assembles the .apkg with the
    Go builder (cmd/mkapkg). No Asustor apkg-tools required.

.EXAMPLE
    ./scripts/package.ps1 -Version 0.7.0
#>
[CmdletBinding()]
param(
    [string]$Version = "0.7.1"
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        $goBin = "C:\Program Files\Go\bin"
        if (Test-Path "$goBin\go.exe") { $env:Path = "$goBin;$env:Path" }
    }

    $dist = Join-Path $repo "dist"
    $stage = Join-Path $dist "pkgroot"
    Remove-Item -Recurse -Force $stage -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force (Join-Path $stage "CONTROL") | Out-Null

    Write-Host "==> Cross-compiling beacon $Version (linux/arm64)" -ForegroundColor Cyan
    $env:GOOS = "linux"; $env:GOARCH = "arm64"; $env:CGO_ENABLED = "0"
    go build -ldflags "-s -w -X main.version=$Version" -o (Join-Path $stage "beacon") ./cmd/beacon
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

    Write-Host "==> Staging package tree" -ForegroundColor Cyan
    Copy-Item (Join-Path $repo "packaging\apkg\CONTROL\*") (Join-Path $stage "CONTROL") -Recurse -Force
    Copy-Item (Join-Path $repo "scripts\beaconctl.sh") $stage -Force
    Copy-Item (Join-Path $repo "beacon.example.toml") $stage -Force

    Write-Host "==> Generating icon" -ForegroundColor Cyan
    go run ./cmd/mkicon (Join-Path $stage "CONTROL\icon.png") 256
    if ($LASTEXITCODE -ne 0) { throw "icon generation failed" }

    # ASUSTOR packages use the .apk extension (they call them "APK files"),
    # even though the internal format is "APKG 2.0".
    $apkg = Join-Path $dist "Beacon_${Version}_arm64.apk"
    Write-Host "==> Assembling $apkg" -ForegroundColor Cyan
    go run ./cmd/mkapkg -root $stage -out $apkg -version $Version
    if ($LASTEXITCODE -ne 0) { throw "mkapkg failed" }

    $size = [math]::Round((Get-Item $apkg).Length / 1MB, 2)
    Write-Host "==> Done: $apkg ($size MB)" -ForegroundColor Green
    Write-Host "    Install on the NAS: App Central > (top-right) Manual Install tab >" -ForegroundColor Green
    Write-Host "    Browse to this .apk > Upload." -ForegroundColor Green
}
finally {
    Pop-Location
}
