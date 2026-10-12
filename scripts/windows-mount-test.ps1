#requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot 'windows-mount.ps1'
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$point = @('Z:', 'Y:', 'X:', 'W:', 'V:') | Where-Object {
    -not (Get-ScheduledTask -TaskName "pkudisk-fuse-$sid-$($_[0])" -ErrorAction SilentlyContinue) -and
    -not (Get-PSDrive -Name $_[0] -ErrorAction SilentlyContinue)
} | Select-Object -First 1
if (-not $point) { throw 'No unused test drive letter is available.' }
$runtime = Join-Path $env:LOCALAPPDATA "pkudisk-fuse\background\$($point[0])"
$fixture = Join-Path $env:TEMP ("pkudisk-background-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $fixture | Out-Null
$source = Join-Path $fixture 'fixture.cs'
$binary = Join-Path $fixture 'fixture & spaced.exe'
$compiler = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
$code = @'
using System;
using System.IO;
using System.Threading;
static class Fixture {
    static int Main(string[] args) {
        if (args.Length != 3 || args[0] != "mount" || args[2] != "--non-interactive") return 9;
        File.WriteAllText(Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "child.pid"),
            System.Diagnostics.Process.GetCurrentProcess().Id.ToString());
        Console.WriteLine("fixture stdout " + args[1]);
        Console.Error.WriteLine("fixture stderr");
        Thread.Sleep(File.Exists(Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "hold")) ? 60000 : 1200);
        return 7;
    }
}
'@
[IO.File]::WriteAllText($source, $code)
& $compiler /nologo /target:exe "/out:$binary" $source
if ($LASTEXITCODE -ne 0) { throw 'Fixture compilation failed.' }
$hostProcess = $null
try {
    & $installer -Action Install -MountPoint $point -Executable $binary
    $taskName = "pkudisk-fuse-$sid-$($point[0])"
    $installed = Get-ScheduledTask -TaskName $taskName
    $xml = [xml](Export-ScheduledTask -TaskName $taskName)
    if ($xml.Task.Principals.Principal.LogonType -ne 'InteractiveToken' -or
        $installed.Principal.RunLevel -ne 'Limited' -or
        $xml.Task.Principals.Principal.UserId -ne $sid -or
        $xml.Task.Triggers.LogonTrigger.UserId -ne [Security.Principal.WindowsIdentity]::GetCurrent().Name -or
        $xml.Task.Settings.ExecutionTimeLimit -ne 'PT0S') { throw 'Incorrect startup settings.' }
    $rules = (Get-Acl -LiteralPath $runtime).GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])
    if ($rules.Count -ne 2 -or @($rules | Where-Object {
        $_.IdentityReference.Value -notin @($sid, 'S-1-5-18') -or $_.IsInherited
    }).Count) { throw 'Background runtime permissions are not private.' }
    $before = @(Get-Process -Name WindowsTerminal, OpenConsole -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Id)
    $launch = [Diagnostics.ProcessStartInfo]::new()
    $launch.FileName = Join-Path $runtime 'mount-host.exe'
    $launch.Arguments = "`"$binary`" $point"
    $launch.UseShellExecute = $false
    foreach ($attempt in 1..3) {
        $logFile = Join-Path $runtime 'mount.log'
        $offset = if (Test-Path -LiteralPath $logFile) { (Get-Item -LiteralPath $logFile).Length } else { 0 }
        Remove-Item -LiteralPath (Join-Path $fixture 'child.pid') -ErrorAction SilentlyContinue
        $hostProcess = [Diagnostics.Process]::Start($launch)
        $deadline = [DateTime]::UtcNow.AddSeconds(15)
        do {
            if ([DateTime]::UtcNow -ge $deadline) { throw 'Fixture did not finish within the test timeout.' }
            if ($hostProcess.HasExited) { throw 'Launcher exited instead of supervising a failed mount.' }
            if ($hostProcess.MainWindowHandle -ne 0) { throw 'Launcher opened a window.' }
            $newWindows = @(Get-Process -Name WindowsTerminal, OpenConsole -ErrorAction SilentlyContinue | Where-Object { $_.Id -notin $before })
            if ($newWindows.Count) { throw 'Background startup opened a terminal process.' }
            $log = if (Test-Path -LiteralPath $logFile) { (Get-Content -LiteralPath $logFile -Raw).Substring([int]$offset) } else { '' }
            Start-Sleep -Milliseconds 50
        } while (-not $log.Contains('Retrying mount in one minute.'))
        foreach ($marker in @("fixture stdout $point", 'fixture stderr', 'Mount exited with code 7.')) {
            if (-not $log.Contains($marker)) { throw "Missing log marker: $marker" }
        }
        $hostProcess.Kill()
        $hostProcess.WaitForExit()
        $hostProcess.Dispose()
        $hostProcess = $null
    }
    # A failed child must be restarted, and stopping the task must stop that child.
    Remove-Item -LiteralPath (Join-Path $fixture 'child.pid')
    & $installer -Action Start -MountPoint $point
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    while (-not (Test-Path -LiteralPath (Join-Path $fixture 'child.pid'))) {
        if ([DateTime]::UtcNow -ge $deadline) { throw 'Scheduled task did not launch its mount child.' }
        Start-Sleep -Milliseconds 100
    }
    $childId = [int](Get-Content -LiteralPath (Join-Path $fixture 'child.pid'))
    New-Item -ItemType File -Path (Join-Path $fixture 'hold') | Out-Null
    Write-Output 'Checking the one-minute failure restart...'
    $deadline = [DateTime]::UtcNow.AddSeconds(75)
    do {
        Start-Sleep -Milliseconds 200
        $restartedId = [int](Get-Content -LiteralPath (Join-Path $fixture 'child.pid'))
        if ([DateTime]::UtcNow -ge $deadline) { throw 'The failed mount was not restarted.' }
    } while ($restartedId -le 0 -or $restartedId -eq $childId)
    $childId = $restartedId
    & $installer -Action Stop -MountPoint $point
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    while (Get-Process -Id $childId -ErrorAction SilentlyContinue) {
        if ([DateTime]::UtcNow -ge $deadline) {
            Get-CimInstance Win32_Process -Filter "Name='mount-host.exe' OR ProcessId=$childId" |
                Select-Object ProcessId, ParentProcessId, ExecutablePath | Format-Table | Out-String | Write-Output
            Get-ScheduledTask -TaskName $taskName | Select-Object TaskName, State | Format-Table | Out-String | Write-Output
            throw 'Stopping the task left an orphan mount.'
        }
        Start-Sleep -Milliseconds 100
    }
    Write-Output 'PASS: private GUI launcher, silent child, literal paths, both log streams, exit codes, user-logon task, failure restart and child cleanup.'
} finally {
    if ($hostProcess) {
        if (-not $hostProcess.HasExited) { $hostProcess.Kill(); $hostProcess.WaitForExit() }
        $hostProcess.Dispose()
    }
    & $installer -Action Remove -MountPoint $point
    Get-CimInstance Win32_Process -Filter "Name='fixture & spaced.exe'" | Where-Object {
        $_.ExecutablePath -eq $binary
    } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
    $fixture = [IO.Path]::GetFullPath($fixture)
    if ([IO.Path]::GetDirectoryName($fixture) -ne [IO.Path]::GetFullPath($env:TEMP).TrimEnd('\') -or
        [IO.Path]::GetFileName($fixture) -notmatch '^pkudisk-background-[a-f0-9]{32}$') { throw 'Unsafe fixture cleanup path.' }
    Remove-Item -LiteralPath $fixture -Recurse
}
