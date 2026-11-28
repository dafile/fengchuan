[CmdletBinding()]
param(
    [string]$Listen = ":8099",
    [string]$SessionTTL = "10m"
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

if ($env:FENGCHUAN_LISTEN) {
    $Listen = $env:FENGCHUAN_LISTEN
}
if ($env:FENGCHUAN_SESSION_TTL) {
    $SessionTTL = $env:FENGCHUAN_SESSION_TTL
}

$baseDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$exeCandidates = @(
    (Join-Path $baseDir "fengchuan.exe"),
    (Join-Path $baseDir "lanchat.exe")
)
$exe = $exeCandidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
if (-not $exe) {
    [System.Windows.Forms.MessageBox]::Show("fengchuan.exe or lanchat.exe was not found.", "FengChuan")
    exit 1
}

$password = [Environment]::GetEnvironmentVariable("FENGCHUAN_PASSWORD")
if ([string]::IsNullOrWhiteSpace($password)) {
    [System.Windows.Forms.MessageBox]::Show("FENGCHUAN_PASSWORD is not set.", "FengChuan")
    exit 1
}

$port = "8099"
if ($Listen -match ":([0-9]+)$") {
    $port = $Matches[1]
}
$healthUrl = "http://127.0.0.1:$port/api/health"
$dataDir = Join-Path $baseDir "data"
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
$stdoutLog = Join-Path $dataDir "service.log"
$stderrLog = Join-Path $dataDir "service-error.log"

$mutex = New-Object System.Threading.Mutex($false, "Local\FengChuanTray")
if (-not $mutex.WaitOne(0)) {
    exit 0
}

$server = $null
$listener = Get-NetTCPConnection -LocalPort ([int]$port) -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
if ($listener) {
    $server = Get-Process -Id $listener.OwningProcess -ErrorAction SilentlyContinue
}
if (-not $server) {
    $server = Start-Process -FilePath $exe `
        -ArgumentList @("-listen", $Listen, "-session-ttl", $SessionTTL) `
        -WorkingDirectory $baseDir `
        -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutLog `
        -RedirectStandardError $stderrLog `
        -PassThru
}

$ready = $false
for ($i = 0; $i -lt 60; $i++) {
    try {
        $health = Invoke-RestMethod -Uri $healthUrl -TimeoutSec 1
        if ($health.ok) {
            $ready = $true
            break
        }
    } catch {
    }
    Start-Sleep -Milliseconds 200
}
if (-not $ready) {
    if ($server -and -not $server.HasExited) {
        Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue
    }
    [System.Windows.Forms.MessageBox]::Show("FengChuan did not start on port $port. Check data\service-error.log.", "FengChuan")
    exit 1
}

$notify = New-Object System.Windows.Forms.NotifyIcon
$iconPath = Join-Path $baseDir "fengchuan.ico"
if (Test-Path -LiteralPath $iconPath -PathType Leaf) {
    $notify.Icon = New-Object System.Drawing.Icon($iconPath)
} else {
    $notify.Icon = [System.Drawing.SystemIcons]::Application
}
$notify.Text = "FengChuan"
$notify.Visible = $true

$form = New-Object System.Windows.Forms.Form
$form.Text = "FengChuan Tray"
$form.ShowInTaskbar = $false
$form.FormBorderStyle = [System.Windows.Forms.FormBorderStyle]::None
$form.WindowState = [System.Windows.Forms.FormWindowState]::Minimized
$form.Opacity = 0
$form.Size = New-Object System.Drawing.Size(1, 1)
$form.Location = New-Object System.Drawing.Point(-10000, -10000)

function Open-FengChuan {
    Start-Process $script:healthUrl
}

function Stop-FengChuanTray {
    if ($script:server -and -not $script:server.HasExited) {
        Stop-Process -Id $script:server.Id -Force -ErrorAction SilentlyContinue
    }
    $script:notify.Visible = $false
    $script:notify.Dispose()
    $script:form.Close()
}

$menu = New-Object System.Windows.Forms.ContextMenuStrip
$openItem = New-Object System.Windows.Forms.ToolStripMenuItem
$openItem.Text = "Open FengChuan"
$openItem.Add_Click({ Open-FengChuan })
$exitItem = New-Object System.Windows.Forms.ToolStripMenuItem
$exitItem.Text = "Stop and Exit"
$exitItem.Add_Click({ Stop-FengChuanTray })
[void]$menu.Items.Add($openItem)
[void]$menu.Items.Add($exitItem)
$notify.ContextMenuStrip = $menu
$notify.Add_MouseDoubleClick({ Open-FengChuan })

$timer = New-Object System.Windows.Forms.Timer
$timer.Interval = 2000
$timer.Add_Tick({
    if (-not $script:server -or $script:server.HasExited) {
        $script:notify.Visible = $false
        $script:notify.Dispose()
        $script:form.Close()
    }
})
$timer.Start()

try {
    [System.Windows.Forms.Application]::Run($form)
} finally {
    $timer.Stop()
    $notify.Visible = $false
    $notify.Dispose()
    $mutex.ReleaseMutex()
    $mutex.Dispose()
}
