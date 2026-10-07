param(
    [switch]$Wait
)

$ErrorActionPreference = 'Stop'

$localCredentialFile = Join-Path $PSScriptRoot 'dingtalk.local.env'
if (Test-Path -LiteralPath $localCredentialFile -PathType Leaf) {
    foreach ($line in Get-Content -LiteralPath $localCredentialFile) {
        if ($line -match '^s*(DINGTALK_CLIENT_ID|DINGTALK_CLIENT_SECRET)s*=s*(.*?)s*$') {
            $name = $Matches[1]
            $value = $Matches[2].Trim().Trim('"').Trim("'")
            if (-not [string]::IsNullOrWhiteSpace($value) -and [string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($name))) {
                Set-Item -Path "Env:$name" -Value $value
            }
        }
    }
}

$required = @('DB_DSN', 'JWT_SECRET')
foreach ($name in $required) {
    if ([string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($name))) {
        throw "缺少必需环境变量 $name。请先在当前 PowerShell 会话中设置后再启动。"
    }
}
if ([Environment]::GetEnvironmentVariable('JWT_SECRET').Length -lt 32) {
    throw 'JWT_SECRET 至少需要 32 个字符。'
}

$hasAccessToken = -not [string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable('DINGTALK_ACCESS_TOKEN'))
$hasClientPair = (-not [string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable('DINGTALK_CLIENT_ID'))) -and
    (-not [string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable('DINGTALK_CLIENT_SECRET')))
if (-not ($hasAccessToken -or $hasClientPair)) {
    throw '请配置 DINGTALK_CLIENT_ID/DINGTALK_CLIENT_SECRET，或配置临时 DINGTALK_ACCESS_TOKEN。'
}

$exe = Join-Path $PSScriptRoot 'checkin-server.exe'
if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) {
    throw "找不到 $exe，请先编译项目。"
}

# 钉钉客户端默认直连；清除当前会话中可能残留的失效代理。
foreach ($name in @('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'http_proxy', 'https_proxy', 'all_proxy')) {
    Remove-Item -Path "Env:$name" -ErrorAction SilentlyContinue
}
if ([string]::IsNullOrWhiteSpace($env:LISTEN_ADDR)) {
    $env:LISTEN_ADDR = '0.0.0.0:8080'
}

if ($Wait) {
    & $exe
    exit $LASTEXITCODE
}

$process = Start-Process -FilePath $exe -WorkingDirectory $PSScriptRoot -PassThru
Write-Output ("checkin-server started (PID {0}, LISTEN_ADDR {1})" -f $process.Id, $env:LISTEN_ADDR)
