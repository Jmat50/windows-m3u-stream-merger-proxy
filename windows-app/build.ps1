$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Resolve-Path (Join-Path $scriptDir "..")
$buildRoot = Join-Path $scriptDir "build"
$serverDir = Join-Path $buildRoot "server"
$serverExe = Join-Path $serverDir "windows-m3u-stream-merger-proxy.exe"
$guiScript = Join-Path $scriptDir "gui_app.py"
$distRoot = Join-Path $scriptDir "dist"
$pyInstallerWork = Join-Path $buildRoot "pyinstaller"

function Get-DesktopProcesses {
    Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -in @("WindowsM3UStreamMergerProxyDesktop.exe", "windows-m3u-stream-merger-proxy.exe") }
}

function Stop-DesktopProcesses {
    $procs = Get-DesktopProcesses
    foreach ($proc in $procs) {
        try {
            Stop-Process -Id $proc.ProcessId -Force -ErrorAction SilentlyContinue
        } catch {
            # Best effort; retry loop below handles stragglers.
        }
    }
}

function Wait-DesktopProcessesStopped {
    $attempt = 0
    while ($attempt -lt 30) {
        $remaining = @(Get-DesktopProcesses)
        if ($remaining.Count -eq 0) {
            return
        }
        Stop-DesktopProcesses
        Start-Sleep -Milliseconds 200
        $attempt++
    }

    $remaining = @(Get-DesktopProcesses | ForEach-Object { "$($_.Name) PID $($_.ProcessId)" })
    if ($remaining.Count -gt 0) {
        throw "Could not stop running desktop/server processes: $($remaining -join ', ')"
    }
}

function Remove-LegacyDistFolders {
    Get-ChildItem -Path $scriptDir -Directory -Filter "dist-new*" -ErrorAction SilentlyContinue |
        ForEach-Object {
            try {
                Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
            } catch {
                # Ignore best-effort cleanup failures for legacy paths.
            }
        }
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go is required to compile windows-m3u-stream-merger-proxy.exe. Install Go then rerun this script."
}

if (-not (Get-Command python -ErrorAction SilentlyContinue)) {
    throw "Python is required to build the desktop app."
}

Write-Host "Stopping running desktop processes..."
Stop-DesktopProcesses
Wait-DesktopProcessesStopped

Write-Host "Cleaning previous output..."
Remove-LegacyDistFolders
if (Test-Path -LiteralPath $distRoot) {
    Remove-Item -LiteralPath $distRoot -Recurse -Force
}

New-Item -ItemType Directory -Force -Path $buildRoot | Out-Null
New-Item -ItemType Directory -Force -Path $serverDir | Out-Null
New-Item -ItemType Directory -Force -Path $distRoot | Out-Null

Write-Host "Building server binary..."
Push-Location $repoRoot
go build -o $serverExe .
Pop-Location

Write-Host "Installing/Updating PyInstaller..."
python -m pip install --upgrade pyinstaller pystray pillow

Write-Host "Packaging GUI executable..."
Push-Location $scriptDir
python -m PyInstaller `
    --noconfirm `
    --clean `
    --windowed `
    --name WindowsM3UStreamMergerProxyDesktop `
    --distpath $distRoot `
    --workpath $pyInstallerWork `
    --specpath $buildRoot `
    --add-binary "$serverExe;server" `
    $guiScript
Pop-Location

Write-Host "Build complete."
Write-Host "Output:" (Join-Path $distRoot "WindowsM3UStreamMergerProxyDesktop")

