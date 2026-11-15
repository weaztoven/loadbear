param(
    [string]$Duration = "10s",
    [string]$Warmup = "5s",
    [int]$Concurrency = 64,
    [int]$Runs = 2
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$Temp = Join-Path ([System.IO.Path]::GetTempPath()) "loadbear-bench-$PID"
$Loadbear = Join-Path $Temp "loadbear.exe"
$Fakebackend = Join-Path $Temp "fakebackend.exe"
$Loadgen = Join-Path $Temp "loadgen.exe"
$script:Processes = New-Object 'System.Collections.Generic.List[System.Diagnostics.Process]'

function Stop-BenchProcesses {
    foreach ($process in @($script:Processes)) {
        try {
            if (-not $process.HasExited) {
                $process.Kill()
                $process.WaitForExit(5000) | Out-Null
            }
        } catch {
            Write-Warning "Could not stop benchmark process $($process.Id): $_"
        }
    }
    $script:Processes.Clear()
}

function Get-FreePort {
    Write-Host "+ `"$Loadgen`" --free-port"
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    $port = $listener.LocalEndpoint.Port
    $listener.Stop()
    return $port
}

function Start-FakeBackend([int]$Port, [string]$Name) {
    $stdout = Join-Path $Temp "$Name.log"
    $stderr = Join-Path $Temp "$Name.err"
    Write-Host "+ `"$Fakebackend`" --port $Port --name `"$Name`""
    $process = Start-Process -FilePath $Fakebackend -ArgumentList @("--port", "$Port", "--name", $Name) `
        -PassThru -NoNewWindow -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $script:Processes.Add($process)
}

function Wait-Http([string]$Url) {
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        foreach ($process in @($script:Processes)) {
            $process.Refresh()
            if ($process.HasExited) {
                throw "Benchmark process $($process.Id) exited before $Url became ready."
            }
        }
        try {
            Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 1 | Out-Null
            return
        } catch {
            Start-Sleep -Milliseconds 200
        }
    }
    throw "Timed out waiting for $Url"
}

function Wait-Healthy([string]$Url, [int]$Count) {
    for ($attempt = 0; $attempt -lt 60; $attempt++) {
        try {
            $status = Invoke-RestMethod -Uri $Url -TimeoutSec 1
            $healthy = @($status.backends | Where-Object { $_.healthy }).Count
            if ($healthy -eq $Count) {
                Write-Host "Admin /status reports $healthy healthy backends."
                return
            }
        } catch {}
        Start-Sleep -Milliseconds 200
    }
    throw "Timed out waiting for $Count healthy backends at $Url"
}

function Run-Loadgen([string]$Url, [int]$RunNumber) {
    Write-Host "Run ${RunNumber}: rate_limit.enabled=false for proxy scenarios"
    Write-Host "+ `"$Loadgen`" --url `"$Url`" --duration `"$Duration`" --warmup `"$Warmup`" --concurrency $Concurrency --method GET --format json"
    $output = & $Loadgen --url $Url --duration $Duration --warmup $Warmup --concurrency $Concurrency --method GET --format json
    if ($LASTEXITCODE -ne 0) {
        throw "loadgen failed with exit code $LASTEXITCODE"
    }
    Write-Output $output
}

function Run-Scenario([string]$Label, [int]$BackendCount, [string]$Strategy, [bool]$Direct) {
    Write-Host ""
    Write-Host "=== Scenario: $Label ==="
    $ports = @()
    for ($index = 1; $index -le $BackendCount; $index++) {
        $ports += Get-FreePort
    }
    for ($index = 0; $index -lt $BackendCount; $index++) {
        Start-FakeBackend -Port $ports[$index] -Name "backend-$($index + 1)"
        Wait-Http "http://127.0.0.1:$($ports[$index])/health"
    }

    if ($Direct) {
        $target = "http://127.0.0.1:$($ports[0])/"
    } else {
        $proxyPort = Get-FreePort
        $adminPort = Get-FreePort
        $configPath = Join-Path $Temp "$Label.yaml"
        $lines = @(
            "listen: `"127.0.0.1:$proxyPort`""
            "admin_listen: `"127.0.0.1:$adminPort`""
            "strategy: `"$Strategy`""
            "log_level: `"error`""
            "backends:"
        )
        for ($index = 0; $index -lt $BackendCount; $index++) {
            $lines += "  - url: `"http://127.0.0.1:$($ports[$index])`""
            $lines += "    weight: 1"
        }
        $lines += @("rate_limit:", "  enabled: false", "health:", "  interval: `"1h`"")
        $config = $lines -join "`n"
        [System.IO.File]::WriteAllText($configPath, $config, [System.Text.UTF8Encoding]::new($false))
        Write-Host "Generated config ($configPath):"
        Write-Host $config
        Write-Host "Proxy config uses rate_limit.enabled=false and log_level=error."
        Write-Host "+ `"$Loadbear`" --config `"$configPath`""
        $log = Join-Path $Temp "loadbear-$Label.log"
        $errorLog = Join-Path $Temp "loadbear-$Label.err"
        $process = Start-Process -FilePath $Loadbear -ArgumentList @("--config", $configPath) `
            -PassThru -NoNewWindow -RedirectStandardOutput $log -RedirectStandardError $errorLog
        $script:Processes.Add($process)
        Wait-Http "http://127.0.0.1:$adminPort/status"
        Wait-Healthy "http://127.0.0.1:$adminPort/status" $BackendCount
        $target = "http://127.0.0.1:$proxyPort/"
    }

    for ($run = 1; $run -le $Runs; $run++) {
        Run-Loadgen -Url $target -RunNumber $run
    }
    Stop-BenchProcesses
}

try {
    New-Item -ItemType Directory -Path $Temp -Force | Out-Null
    Set-Location $Root
    $os = Get-CimInstance Win32_OperatingSystem
    $processors = @(Get-CimInstance Win32_Processor)
    $cores = ($processors | Measure-Object -Property NumberOfCores -Sum).Sum
    Write-Host "Benchmark machine information"
    Write-Host "Date: $((Get-Date).ToString('o'))"
    Write-Host "OS: $($os.Caption) $($os.Version)"
    Write-Host "CPU: $($processors[0].Name)"
    Write-Host "Physical CPU cores: $cores"
    Write-Host "Logical CPU cores: $([Environment]::ProcessorCount)"
    Write-Host "Go: $(& go version)"
    Write-Host "Duration: $Duration; warmup: $Warmup; concurrency: $Concurrency; runs per scenario: $Runs"
    Write-Host ""

    Write-Host "+ go build -o `"$Loadbear`" ./cmd/loadbear"
    & go build -o $Loadbear ./cmd/loadbear
    if ($LASTEXITCODE -ne 0) { throw "go build loadbear failed" }
    Write-Host "+ go build -o `"$Fakebackend`" ./cmd/fakebackend"
    & go build -o $Fakebackend ./cmd/fakebackend
    if ($LASTEXITCODE -ne 0) { throw "go build fakebackend failed" }
    Write-Host "+ go build -o `"$Loadgen`" ./cmd/loadgen"
    & go build -o $Loadgen ./cmd/loadgen
    if ($LASTEXITCODE -ne 0) { throw "go build loadgen failed" }

    Run-Scenario "direct-one-backend" 1 "round_robin" $true
    Run-Scenario "proxy-one-backend" 1 "round_robin" $false
    Run-Scenario "proxy-three-round-robin" 3 "round_robin" $false
    Run-Scenario "proxy-three-smooth-weighted-round-robin" 3 "smooth_weighted_round_robin" $false
    Run-Scenario "proxy-three-least-connections" 3 "least_connections" $false
    Run-Scenario "proxy-three-ip-hash" 3 "ip_hash" $false
} finally {
    Stop-BenchProcesses
    if (Test-Path -LiteralPath $Temp) {
        Remove-Item -LiteralPath $Temp -Recurse -Force
    }
}
