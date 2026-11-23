# billgo 安装脚本（Windows）
#
#   irm https://raw.githubusercontent.com/wong4ga1ming4/billgo/main/scripts/install.ps1 | iex
#
# 可覆盖：
#   .\install.ps1 -Version v1.0.0
#   .\install.ps1 -InstallDir C:\tools\billgo
param(
    [string]$Version = "",
    [string]$InstallDir = "$env:LOCALAPPDATA\billgo\bin",
    [string]$BaseUrl = "https://github.com/wong4ga1ming4/billgo/releases/download"
)

$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo = "wong4ga1ming4/billgo"

switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { $Arch = "amd64" }
    "ARM64" { $Arch = "arm64" }
    default { throw "不支持的架构: $env:PROCESSOR_ARCHITECTURE" }
}

if (-not $Version) {
    $rel = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
    $Version = $rel.tag_name
}
if (-not $Version) { throw "无法获取最新版本，可用 -Version v1.0.0 指定" }

$Asset = "billgo_${Version}_windows_${Arch}.zip"
$Base = "$BaseUrl/$Version"
$Tmp = Join-Path $env:TEMP ("billgo-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $Tmp | Out-Null

try {
    Write-Host "下载 $Asset ..."
    $Zip = Join-Path $Tmp $Asset
    Invoke-WebRequest "$Base/$Asset" -OutFile $Zip -UseBasicParsing
    $Checksums = Join-Path $Tmp "checksums.txt"
    Invoke-WebRequest "$Base/checksums.txt" -OutFile $Checksums -UseBasicParsing

    $Expected = Select-String -Path $Checksums -Pattern ([regex]::Escape($Asset)) |
        ForEach-Object { $_.Line.Split()[0] } | Select-Object -First 1
    if (-not $Expected) { throw "校验文件里没有 $Asset" }
    $Actual = (Get-FileHash $Zip -Algorithm SHA256).Hash.ToLower()
    if ($Actual -ne $Expected.ToLower()) {
        throw "校验失败: 期望 $Expected，实际 $Actual"
    }

    Expand-Archive -Path $Zip -DestinationPath $Tmp -Force
    $Src = Join-Path $Tmp "billgo_${Version}_windows_${Arch}"
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item (Join-Path $Src "billgod.exe") (Join-Path $InstallDir "billgod.exe") -Force
    Copy-Item (Join-Path $Src "billgo.exe") (Join-Path $InstallDir "billgo.exe") -Force
    Copy-Item (Join-Path $Src "billui.exe") (Join-Path $InstallDir "billui.exe") -Force

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($UserPath -notlike "*$InstallDir*") {
        [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
        Write-Host "已把 $InstallDir 加进用户 PATH，重新打开终端后生效。"
    }

    Write-Host "已安装 $Version 到 $InstallDir"
    & (Join-Path $InstallDir "billgo.exe") version
}
finally {
    Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}
