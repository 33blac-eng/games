<#
.SYNOPSIS
  ЧЕРНЕТКА. Замінює oo-agent.exe на ПК з бекапом, перезапускає заплановану задачу,
  перевіряє процес і рядок у лозі; на провалі — відкат.

.DESCRIPTION
  🔴 ПОРЯДОК ПАРИ (PLAN-waves.md): агент і хаб збираються з ОДНОГО коміту і
  їдуть РАЗОМ, і ХАБ ПЕРШИЙ. Новий агент проти старого хаба флапає (~10 с,
  сторож живості). Тому скрипт вимагає -ExpectedHubVersion і порівнює з
  VERSION поруч із бінарем: якщо хаб ще не викочено з цього коміту
  (deploy/hub-deploy.sh), агент НЕ ставиться.

  Запуск (адміністратором, на кожному ПК або через власний механізм розкочування):
    .\agent-deploy.ps1 -NewExe .\oo-agent-windows-amd64.exe -VersionFile .\VERSION `
        -ExpectedHubVersion <sha, що зараз на хабі> -Sha256 <з SHA256SUMS>

  Повторний запуск з тим самим бінарем нічого не міняє (лише перевірка).
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Mandatory = $true)] [string] $NewExe,
    [Parameter(Mandatory = $true)] [string] $VersionFile,
    [Parameter(Mandatory = $true)] [string] $ExpectedHubVersion,
    [Parameter(Mandatory = $true)] [string] $Sha256,
    [string] $InstallPath = 'C:\Program Files\oo-screen\oo-agent.exe',
    [string] $TaskName = 'oo-agent',
    [string] $LogPath = 'C:\ProgramData\oo-screen\oo-agent.log',
    # Рядок, що означає «агент піднявся і домовився з хабом».
    [string] $OkPattern = 'oo-agent: webrtc PC state: connected',
    [int] $TimeoutSec = 60
)
$ErrorActionPreference = 'Stop'

function Say($m) { Write-Host "agent-deploy: $m" }

# --- 0. перевірки до будь-яких змін ---
$ver = (Get-Content -Raw $VersionFile).Trim()
if ($ver -ne $ExpectedHubVersion.Trim()) {
    throw "агент $ver, а хаб $ExpectedHubVersion — пара різна. Спершу хаб з $ver (hub-deploy.sh), потім агенти."
}
$got = (Get-FileHash -Algorithm SHA256 $NewExe).Hash.ToLower()
if ($got -ne $Sha256.ToLower()) { throw "SHA256 $NewExe = $got, очікувалось $Sha256" }

if ((Test-Path $InstallPath) -and ((Get-FileHash -Algorithm SHA256 $InstallPath).Hash.ToLower() -eq $got)) {
    Say "вже стоїть $ver — лише перевірка процесу"
    if (-not (Get-Process -Name ([IO.Path]::GetFileNameWithoutExtension($InstallPath)) -ErrorAction SilentlyContinue)) {
        Start-ScheduledTask -TaskName $TaskName
    }
    exit 0
}

$backup = "$InstallPath.bak-$(Get-Date -Format yyyyMMdd-HHmmss)"
$procName = [IO.Path]::GetFileNameWithoutExtension($InstallPath)

function Stop-Agent {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Get-Process -Name $procName -ErrorAction SilentlyContinue | Stop-Process -Force
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Process -Name $procName -ErrorAction SilentlyContinue) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 300 }
}

function Test-Agent([long] $logOffset) {
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 2
        if (-not (Get-Process -Name $procName -ErrorAction SilentlyContinue)) { continue }
        if (Test-Path $LogPath) {
            $fs = [IO.File]::Open($LogPath, 'Open', 'Read', 'ReadWrite')
            try {
                if ($fs.Length -lt $logOffset) { $logOffset = 0 } # лог ротовано
                $null = $fs.Seek($logOffset, 'Begin')
                $tail = (New-Object IO.StreamReader($fs)).ReadToEnd()
            } finally { $fs.Dispose() }
            if ($tail -match 'panic:|fatal error:') { Say 'у лозі panic'; return $false }
            if ($tail -match [regex]::Escape($OkPattern)) { return $true }
        }
    }
    Say "за ${TimeoutSec}s не побачив процес + '$OkPattern'"
    return $false
}

if (-not $PSCmdlet.ShouldProcess($InstallPath, "замінити на $ver")) { exit 0 }

# --- 1. заміна з бекапом ---
$offset = 0; if (Test-Path $LogPath) { $offset = (Get-Item $LogPath).Length }
Stop-Agent
if (Test-Path $InstallPath) { Copy-Item $InstallPath $backup -Force; Say "бекап -> $backup" }
Copy-Item $NewExe "$InstallPath.new" -Force
Move-Item "$InstallPath.new" $InstallPath -Force
Start-ScheduledTask -TaskName $TaskName

# --- 2. перевірка ---
if (Test-Agent $offset) { Say "OK: $ver працює"; exit 0 }

# --- 3. відкат ---
if (-not (Test-Path $backup)) { throw "$ver не піднявся, а бекапу нема — потрібне втручання" }
Say "ВІДКАТ на $backup"
$offset = 0; if (Test-Path $LogPath) { $offset = (Get-Item $LogPath).Length }
Stop-Agent
Copy-Item $backup $InstallPath -Force
Start-ScheduledTask -TaskName $TaskName
if (Test-Agent $offset) { throw "$ver не піднявся; відкочено, попередній агент працює" }
throw "$ver не піднявся; відкат ТЕЖ не піднявся — потрібне втручання"
