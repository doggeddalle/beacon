<#
.SYNOPSIS
    Cross-compile Beacon for the Asustor NAS (linux/arm64) and deploy it over SSH.

.DESCRIPTION
    Builds a static ARM64 binary, copies it (plus the example config on first
    run) to the NAS via scp, and restarts it over ssh. Requires SSH to be
    enabled on the NAS (ADM: Services > Terminal / SSH) and an OpenSSH client
    on Windows (built into Windows 10/11).

.EXAMPLE
    ./scripts/deploy.ps1 -NasHost 192.168.1.50 -NasUser admin

.EXAMPLE
    ./scripts/deploy.ps1 -NasHost nas.local -NasUser admin -RemoteDir /volume1/.beacon -Run
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$NasHost,
    [Parameter(Mandatory = $true)][string]$NasUser,
    [int]$SshPort = 22,
    [string]$RemoteDir = "/volume1/.beacon",
    [string]$Version = "0.6.0-p6",
    # Optional: path to a static linux/arm64 ffprobe binary to install next to
    # beacon (enables duration/resolution metadata). Only needed if the NAS
    # doesn't already have ffprobe on PATH.
    [string]$FFprobe = "",
    # If set, (re)starts beacon on the NAS after copying.
    [switch]$Run
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
    # Ensure go is reachable even if PATH wasn't refreshed since install.
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        $goBin = "C:\Program Files\Go\bin"
        if (Test-Path "$goBin\go.exe") { $env:Path = "$goBin;$env:Path" }
    }

    Write-Host "==> Building beacon $Version for linux/arm64" -ForegroundColor Cyan
    $env:GOOS = "linux"; $env:GOARCH = "arm64"; $env:CGO_ENABLED = "0"
    $bin = Join-Path $repo "dist\beacon"
    New-Item -ItemType Directory -Force (Split-Path $bin) | Out-Null
    go build -ldflags "-s -w -X main.version=$Version" -o $bin ./cmd/beacon
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

    $target = "$NasUser@$NasHost"
    $exampleLocal = Join-Path $repo "beacon.example.toml"

    Write-Host "==> Ensuring $RemoteDir exists on $target" -ForegroundColor Cyan
    ssh -p $SshPort $target "mkdir -p '$RemoteDir'"

    # Copy the binary to a temp name: this succeeds even while the old instance
    # is still running (overwriting a running executable directly fails with
    # ETXTBSY). We swap it into place with an atomic mv below. The control
    # script (beaconctl.sh) handles start/stop portably on the NAS.
    Write-Host "==> Copying files" -ForegroundColor Cyan
    $ctlLocal = Join-Path $PSScriptRoot "beaconctl.sh"
    scp -P $SshPort $bin "${target}:$RemoteDir/beacon.new"
    scp -P $SshPort $exampleLocal "${target}:$RemoteDir/beacon.example.toml"
    scp -P $SshPort $ctlLocal "${target}:$RemoteDir/beaconctl.sh"

    if ($FFprobe -ne "") {
        if (-not (Test-Path $FFprobe)) { throw "ffprobe not found at: $FFprobe" }
        Write-Host "==> Installing ffprobe" -ForegroundColor Cyan
        scp -P $SshPort $FFprobe "${target}:$RemoteDir/ffprobe"
        ssh -p $SshPort $target "chmod +x '$RemoteDir/ffprobe'"
    }

    # Finalise with a SINGLE-LINE remote command. Windows PowerShell 5.1 mangles
    # multi-line arguments passed to native ssh.exe, so everything is chained
    # with && on one line: normalise the control script's line endings, then let
    # beaconctl.sh do the binary swap / config seed (and restart when asked).
    $cmd = "cd '$RemoteDir' && tr -d '\015' < beaconctl.sh > beaconctl.tmp && mv beaconctl.tmp beaconctl.sh && chmod +x beaconctl.sh && sh beaconctl.sh install"
    if ($Run) {
        $cmd += " && sh beaconctl.sh restart"
    }

    Write-Host "==> Installing$(if ($Run) {' and restarting'})" -ForegroundColor Cyan
    ssh -p $SshPort $target $cmd

    if ($Run) {
        Write-Host "==> Tail the log with: ssh -p $SshPort $target 'tail -f $RemoteDir/beacon.log'" -ForegroundColor Green
    } else {
        Write-Host "==> Deployed. To run it on the NAS:" -ForegroundColor Green
        Write-Host "    ssh -p $SshPort $target 'cd $RemoteDir && ./beacon -config beacon.toml'"
    }
}
finally {
    Pop-Location
}
