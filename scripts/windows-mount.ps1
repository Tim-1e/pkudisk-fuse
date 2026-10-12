#requires -Version 5.1
[CmdletBinding()]
param(
    [ValidateSet('Install', 'Start', 'Stop', 'Status', 'Remove')]
    [string]$Action = 'Status',
    [ValidatePattern('^[A-Za-z]:$')]
    [string]$MountPoint = 'P:',
    [string]$Executable
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'This script requires Windows.' }
if (-not $Executable) { $Executable = Join-Path $PSScriptRoot '..\pkudisk-fuse.exe' }
$MountPoint = $MountPoint.ToUpperInvariant()
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$taskName = "pkudisk-fuse-$sid-$($MountPoint[0])"
$runtime = Join-Path $env:LOCALAPPDATA "pkudisk-fuse\background\$($MountPoint[0])"
$launcher = Join-Path $runtime 'mount-host.exe'
$logPath = Join-Path $runtime 'mount.log'
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue

switch ($Action) {
    'Install' {
        if ($task -and $task.State -eq 'Running') {
            throw 'Close files on the mounted drive and run -Action Stop before reinstalling.'
        }
        $Executable = (Get-Item -LiteralPath $Executable -ErrorAction Stop).FullName
        if ([IO.Path]::GetExtension($Executable) -ne '.exe') { throw '-Executable must be an executable file.' }
        $compiler = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
        if (-not (Test-Path -LiteralPath $compiler)) { throw 'The Windows .NET Framework C# compiler is required.' }
        New-Item -ItemType Directory -Path $runtime -Force | Out-Null
        if ((Get-Item -LiteralPath $runtime).Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw 'The background directory must not be a reparse point.'
        }
        & icacls.exe $runtime '/inheritance:r' '/grant:r' "*${sid}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Could not restrict the background directory.' }
        $rules = (Get-Acl -LiteralPath $runtime).GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])
        if (@($rules | Where-Object {
            $_.IdentityReference.Value -notin @($sid, 'S-1-5-18') -or $_.AccessControlType -ne 'Allow'
        }).Count) { throw 'The background directory has unexpected permissions.' }
        # A GUI entry point avoids the outer console; CreateNoWindow hides the actual CLI.
        $source = Join-Path $runtime 'mount-host.cs'
        $code = @'
using System;
using System.ComponentModel;
using System.Diagnostics;
using System.IO;
using System.Reflection;
using System.Runtime.InteropServices;
using System.Threading;

static class MountHost {
    [StructLayout(LayoutKind.Sequential)] struct BasicLimits {
        public long ProcessTime, JobTime;
        public uint Flags;
        public UIntPtr MinWorkingSet, MaxWorkingSet;
        public uint ActiveProcesses;
        public UIntPtr Affinity;
        public uint Priority, Scheduling;
    }
    [StructLayout(LayoutKind.Sequential)] struct ExtendedLimits {
        public BasicLimits Basic;
        public ulong ReadOperations, WriteOperations, OtherOperations, ReadBytes, WriteBytes, OtherBytes;
        public UIntPtr ProcessMemory, JobMemory, PeakProcessMemory, PeakJobMemory;
    }
    [DllImport("kernel32.dll", SetLastError = true)] static extern IntPtr CreateJobObject(IntPtr attributes, string name);
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool SetInformationJobObject(IntPtr job, int kind, ref ExtendedLimits info, int size);
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
    static int Main(string[] args) {
        if (args.Length != 2) return 2;
        IntPtr job = IntPtr.Zero;
        try {
            job = CreateJobObject(IntPtr.Zero, null);
            var limits = new ExtendedLimits { Basic = new BasicLimits { Flags = 0x2000 } };
            if (job == IntPtr.Zero || !SetInformationJobObject(job, 9, ref limits, Marshal.SizeOf(limits))) return 1;
            string directory = Path.GetDirectoryName(Assembly.GetExecutingAssembly().Location);
            using (var log = new StreamWriter(Path.Combine(directory, "mount.log"), true)) {
                log.AutoFlush = true;
                while (true) {
                  try {
                    using (var child = new Process()) {
                        child.StartInfo = new ProcessStartInfo(args[0], "mount " + args[1] + " --non-interactive") {
                            WorkingDirectory = Path.GetDirectoryName(args[0]),
                            UseShellExecute = false,
                            CreateNoWindow = true,
                            RedirectStandardInput = true,
                            RedirectStandardOutput = true,
                            RedirectStandardError = true
                        };
                        DataReceivedEventHandler record = (sender, line) => {
                            try { if (line.Data != null) lock (log) log.WriteLine(line.Data); }
                            catch { try { child.Kill(); } catch { } }
                        };
                        child.OutputDataReceived += record;
                        child.ErrorDataReceived += record;
                        child.Start();
                        try {
                            // Closing the supervisor's job also stops its child after task termination.
                            if (!AssignProcessToJobObject(job, child.Handle)) throw new Win32Exception(Marshal.GetLastWin32Error());
                            log.WriteLine("{0:o} Started mount at {1}; PID {2}.", DateTimeOffset.Now, args[1], child.Id);
                            child.StandardInput.Close();
                            child.BeginOutputReadLine();
                            child.BeginErrorReadLine();
                            child.WaitForExit();
                            log.WriteLine("{0:o} Mount exited with code {1}.", DateTimeOffset.Now, child.ExitCode);
                            if (child.ExitCode == 0) return 0;
                        } finally {
                            if (!child.HasExited) { child.Kill(); child.WaitForExit(); }
                        }
                    }
                  } catch (Exception error) {
                    log.WriteLine("{0:o} Mount failed: {1}", DateTimeOffset.Now, error.Message);
                  }
                  log.WriteLine("{0:o} Retrying mount in one minute.", DateTimeOffset.Now);
                  Thread.Sleep(60000);
                }
            }
        } catch { return 1; }
        finally { if (job != IntPtr.Zero) CloseHandle(job); }
    }
}
'@
        [IO.File]::WriteAllText($source, $code, [Text.UTF8Encoding]::new($false))
        try {
            & $compiler /nologo /target:winexe "/out:$launcher" $source
            if ($LASTEXITCODE -ne 0) { throw "Launcher compilation failed ($LASTEXITCODE)." }
        } finally {
            Remove-Item -LiteralPath $source
        }
        $entry = New-ScheduledTaskAction -Execute $launcher -Argument "`"$Executable`" $MountPoint" -WorkingDirectory $runtime
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $sid
        $trigger.Delay = 'PT30S'
        $principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
            -StartWhenAvailable `
            -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -Hidden
        Register-ScheduledTask -TaskName $taskName -Action $entry -Trigger $trigger -Principal $principal `
            -Settings $settings -Description "Mount PKU NetDisk at $MountPoint in the current user's session." -Force | Out-Null
        Write-Output "Installed login startup for $MountPoint. Run -Action Start to mount now. Log: $logPath"
    }
    'Start' {
        if (-not $task) { throw 'Run -Action Install first.' }
        Start-ScheduledTask -TaskName $taskName
        Write-Output "Started $taskName. Log: $logPath"
    }
    'Stop' {
        if ($task) { Stop-ScheduledTask -TaskName $taskName }
        Write-Output "Stop requested for $taskName."
    }
    'Remove' {
        if ($task) {
            Stop-ScheduledTask -TaskName $taskName
            Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
        }
        Write-Output "Removed login startup for $MountPoint. Logs and saved accounts are retained."
    }
    'Status' {
        if ($task) {
            $info = Get-ScheduledTaskInfo -TaskName $taskName
            [pscustomobject]@{ TaskName = $taskName; State = $task.State; LastTaskResult = $info.LastTaskResult; LogPath = $logPath }
        } else { Write-Output "No login startup installed for $MountPoint." }
    }
}
