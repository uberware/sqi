# SPDX-License-Identifier: AGPL-3.0-or-later

# Two-tier Windows run-as-user isolation harness.
#
# Tier 1 (TestIsolationWindows_*) runs directly: elevated Administrator is
# enough for account creation, all NTFS ACL work, and impersonated access
# checks.
#
# Tier 2 (TestIsolationWindowsSystem_*) must run as SYSTEM, because
# CreateProcessAsUser requires SeAssignPrimaryTokenPrivilege, which
# administrators do not hold. A scheduled task registered with /ru SYSTEM is
# the mechanism; it is detached, so stdout and the exit code travel through
# files.
#
# Exits 0 with a message when not elevated, mirroring how `make test-isolation`
# exits 0 when Docker is missing. A skip therefore verifies NOTHING — CI
# asserts each test by name for exactly this reason.

$ErrorActionPreference = 'Stop'

# Re-launch under the native 64-bit PowerShell when started from a 32-bit
# process. On 64-bit Windows the LocalAccounts module (Get-LocalUser,
# New-LocalUser) exists ONLY under System32; SysWOW64 ships no copy of it. A
# 32-bit parent that runs "powershell" gets SysWOW64 via the WOW64 file-system
# redirector, so every account cmdlet below fails with CommandNotFound. That is
# not hypothetical: GNU make for Windows (ezwinports) is a 32-bit binary, so
# `make test-isolation-windows` hits this on a stock developer machine. CI
# invokes this script from 64-bit pwsh and therefore can never catch it.
#
# Sysnative is the reverse alias: visible only to 32-bit processes, it reaches
# the real System32. The relaunch inherits this process's token, so an elevated
# shell stays elevated and the check below still sees an administrator.
if (-not [Environment]::Is64BitProcess -and [Environment]::Is64BitOperatingSystem) {
    $native = Join-Path $env:SystemRoot 'Sysnative\WindowsPowerShell\v1.0\powershell.exe'
    if (-not (Test-Path $native)) {
        throw "started from a 32-bit shell and the native 64-bit PowerShell was not found at $native"
    }
    & $native -NoProfile -ExecutionPolicy Bypass -File $PSCommandPath @args
    exit $LASTEXITCODE
}

function Test-Elevated {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $p = New-Object Security.Principal.WindowsPrincipal($id)
    return $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if (-not (Test-Elevated)) {
    Write-Host 'not elevated - skipping windows isolation suite'
    Write-Host 'run from an elevated PowerShell to exercise it'
    exit 0
}

$repo     = Split-Path -Parent $PSScriptRoot
$work     = Join-Path $env:TEMP ('sqi-iso-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
$binary   = Join-Path $work 'isolation.test.exe'
$taskName = 'sqi-isolation-system-tier'
$userA    = 'sqi-iso-a'
$userB    = 'sqi-iso-b'

function New-RandomPassword {
    # 24 random chars, plus a fixed 4-char suffix that guarantees at least
    # one lower/upper/digit/symbol regardless of what the random draw
    # produced -- so this always satisfies a default 3-of-4 complexity
    # policy.
    #
    # The charset intentionally excludes '%': this password is delivered to
    # the SYSTEM-tier process via a `set VAR=<password>` line in a generated
    # .cmd file (see the tier-2 comment below), and cmd.exe percent-expands
    # %name% pairs at PARSE time -- even inside a `set` value -- silently
    # dropping an undefined %ref% or substituting a real environment
    # variable. Do NOT widen this charset to include '%' or any other
    # cmd.exe metacharacter (& | < > ^ "); doing so reintroduces
    # intermittent, hard-to-diagnose password corruption in every later
    # task that depends on this harness.
    $chars = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#$'
    -join (1..24 | ForEach-Object { $chars[(Get-Random -Maximum $chars.Length)] }) + 'aA1!'
}

# The isolation provider logs its run-as-user accounts on with
# LOGON32_LOGON_BATCH (internal/worker/isolation/provider_windows.go), and
# Windows refuses a batch logon for any account lacking SeBatchLogonRight
# ("Log on as a batch job") with ERROR_LOGON_TYPE_NOT_GRANTED. Default
# workstation policy grants that right to Administrators, Backup Operators and
# Performance Log Users only -- never to a plain standard user -- so freshly
# created accounts like sqi-iso-a/-b cannot be logged on until it is granted.
# This is a REAL operator requirement, not a test artifact: see the Windows
# section of docs/worker-configuration.md.
#
# LsaAddAccountRights/LsaRemoveAccountRights are used rather than a
# `secedit /export` + `/configure` round-trip because they touch exactly one
# right for exactly one SID. secedit re-applies the ENTIRE USER_RIGHTS area
# from a regenerated template, which is far too blunt to point at a developer's
# own machine. There is no built-in cmdlet for either API.
Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;

public static class SqiLsaRights
{
    [StructLayout(LayoutKind.Sequential)]
    private struct LSA_UNICODE_STRING
    {
        public ushort Length;
        public ushort MaximumLength;
        public IntPtr Buffer;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct LSA_OBJECT_ATTRIBUTES
    {
        public int Length;
        public IntPtr RootDirectory;
        public IntPtr ObjectName;
        public uint Attributes;
        public IntPtr SecurityDescriptor;
        public IntPtr SecurityQualityOfService;
    }

    [DllImport("advapi32.dll", SetLastError = true)]
    private static extern uint LsaOpenPolicy(IntPtr SystemName,
        ref LSA_OBJECT_ATTRIBUTES ObjectAttributes, uint DesiredAccess, out IntPtr PolicyHandle);

    [DllImport("advapi32.dll", SetLastError = true)]
    private static extern uint LsaAddAccountRights(IntPtr PolicyHandle, byte[] AccountSid,
        LSA_UNICODE_STRING[] UserRights, uint CountOfRights);

    [DllImport("advapi32.dll", SetLastError = true)]
    private static extern uint LsaRemoveAccountRights(IntPtr PolicyHandle, byte[] AccountSid,
        [MarshalAs(UnmanagedType.U1)] bool AllRights, LSA_UNICODE_STRING[] UserRights, uint CountOfRights);

    [DllImport("advapi32.dll")]
    private static extern uint LsaClose(IntPtr PolicyHandle);

    [DllImport("advapi32.dll")]
    private static extern int LsaNtStatusToWinError(uint Status);

    // POLICY_CREATE_ACCOUNT | POLICY_LOOKUP_NAMES -- what LsaAddAccountRights
    // requires, and a superset of LsaRemoveAccountRights' POLICY_LOOKUP_NAMES.
    private const uint PolicyAccess = 0x00000010 | 0x00000800;

    private static IntPtr OpenPolicy()
    {
        LSA_OBJECT_ATTRIBUTES attrs = new LSA_OBJECT_ATTRIBUTES();
        attrs.Length = Marshal.SizeOf(typeof(LSA_OBJECT_ATTRIBUTES));
        IntPtr handle;
        uint status = LsaOpenPolicy(IntPtr.Zero, ref attrs, PolicyAccess, out handle);
        if (status != 0) { throw new InvalidOperationException("LsaOpenPolicy: " + Describe(status)); }
        return handle;
    }

    // The LSA APIs return an NTSTATUS, not a Win32 error, so the code has to
    // be translated before it means anything to a reader.
    private static string Describe(uint status)
    {
        int code = LsaNtStatusToWinError(status);
        return new Win32Exception(code).Message + " (win32 " + code + ")";
    }

    private static LSA_UNICODE_STRING[] RightArray(string right)
    {
        LSA_UNICODE_STRING s = new LSA_UNICODE_STRING();
        s.Buffer = Marshal.StringToHGlobalUni(right);
        // Lengths are in BYTES: Length excludes the terminating null,
        // MaximumLength includes it.
        s.Length = (ushort)(right.Length * 2);
        s.MaximumLength = (ushort)((right.Length + 1) * 2);
        return new LSA_UNICODE_STRING[] { s };
    }

    public static void Grant(byte[] sid, string right)
    {
        IntPtr policy = OpenPolicy();
        LSA_UNICODE_STRING[] rights = RightArray(right);
        try
        {
            uint status = LsaAddAccountRights(policy, sid, rights, 1);
            if (status != 0) { throw new InvalidOperationException("LsaAddAccountRights: " + Describe(status)); }
        }
        finally
        {
            Marshal.FreeHGlobal(rights[0].Buffer);
            LsaClose(policy);
        }
    }

    public static void Revoke(byte[] sid, string right)
    {
        IntPtr policy = OpenPolicy();
        LSA_UNICODE_STRING[] rights = RightArray(right);
        try
        {
            // AllRights false: remove only the named right, leaving anything
            // else the account holds alone.
            uint status = LsaRemoveAccountRights(policy, sid, false, rights, 1);
            if (status != 0) { throw new InvalidOperationException("LsaRemoveAccountRights: " + Describe(status)); }
        }
        finally
        {
            Marshal.FreeHGlobal(rights[0].Buffer);
            LsaClose(policy);
        }
    }
}
'@

function Get-AccountSidBytes([string]$name) {
    $sid = (New-Object Security.Principal.NTAccount($name)).Translate([Security.Principal.SecurityIdentifier])
    $bytes = New-Object byte[] $sid.BinaryLength
    $sid.GetBinaryForm($bytes, 0)
    # Leading comma: without it PowerShell unrolls the array on return and the
    # caller gets a stream of bytes instead of the byte[] the P/Invoke needs.
    return ,$bytes
}

function Grant-BatchLogonRight([string]$name) {
    [SqiLsaRights]::Grant((Get-AccountSidBytes $name), 'SeBatchLogonRight')
}

# `reg unload` (used to release a stranded profile hive in Remove-TestProfile)
# requires SeRestorePrivilege and SeBackupPrivilege. An elevated Administrator
# token HOLDS both by default policy but leaves them DISABLED, and reg.exe does
# not enable them itself -- so without this the unload fails with "A required
# privilege is not held by the client" and the hive, plus the profile directory
# it locks, survives every run. Enabling a privilege already present in the
# token needs no elevation beyond what the token already carries; it just flips
# the enabled bit.
Add-Type -Namespace SqiPriv -Name Token -MemberDefinition @'
[DllImport("advapi32.dll", SetLastError = true)]
static extern bool OpenProcessToken(IntPtr process, uint access, out IntPtr token);

[DllImport("advapi32.dll", SetLastError = true)]
static extern bool LookupPrivilegeValue(string system, string name, out long luid);

[DllImport("advapi32.dll", SetLastError = true)]
static extern bool AdjustTokenPrivileges(IntPtr token, bool disableAll,
    ref TOKEN_PRIVILEGE newState, int bufferLength, IntPtr previous, IntPtr returnLength);

[DllImport("kernel32.dll")]
static extern IntPtr GetCurrentProcess();

// Pack = 4 is load-bearing, not cosmetic. TOKEN_PRIVILEGES is DWORD Count
// followed immediately by an 8-byte LUID at offset 4. Under the default
// managed layout the 8-byte-aligned Luid field lands at offset 8, with 4
// bytes of padding after Count -- a 24-byte struct where Windows expects a
// packed 16-byte one, so AdjustTokenPrivileges reads a garbage LUID and
// "succeeds" while enabling nothing (ERROR_NOT_ALL_ASSIGNED). Pack = 4 puts
// Luid at offset 4 and the size back to 16. Without it this fails even for a
// privilege the token plainly holds.
[StructLayout(LayoutKind.Sequential, Pack = 4)]
struct TOKEN_PRIVILEGE { public int Count; public long Luid; public int Attributes; }

// TOKEN_ADJUST_PRIVILEGES | TOKEN_QUERY; SE_PRIVILEGE_ENABLED.
const uint TokenAccess = 0x0020 | 0x0008;
const int SePrivilegeEnabled = 0x00000002;

public static void Enable(string name)
{
    IntPtr token;
    if (!OpenProcessToken(GetCurrentProcess(), TokenAccess, out token))
    { throw new Win32Exception(Marshal.GetLastWin32Error(), "OpenProcessToken"); }
    long luid;
    if (!LookupPrivilegeValue(null, name, out luid))
    { throw new Win32Exception(Marshal.GetLastWin32Error(), "LookupPrivilegeValue(" + name + ")"); }
    TOKEN_PRIVILEGE tp = new TOKEN_PRIVILEGE();
    tp.Count = 1; tp.Luid = luid; tp.Attributes = SePrivilegeEnabled;
    if (!AdjustTokenPrivileges(token, false, ref tp, 0, IntPtr.Zero, IntPtr.Zero))
    { throw new Win32Exception(Marshal.GetLastWin32Error(), "AdjustTokenPrivileges"); }
    // AdjustTokenPrivileges "succeeds" (returns true) even when it assigned
    // nothing, setting ERROR_NOT_ALL_ASSIGNED -- which is the case that
    // matters here: the token simply does not hold the privilege to enable.
    int err = Marshal.GetLastWin32Error();
    if (err != 0) { throw new Win32Exception(err, "AdjustTokenPrivileges(" + name + ") did not assign the privilege"); }
}
'@ -UsingNamespace 'System.ComponentModel'

# Idempotent: enables the two privileges reg unload needs, once. A failure here
# is not fatal -- the unload will simply fail later with its own clear message,
# and on a machine whose policy genuinely withholds these from Administrators
# there is nothing this could do anyway.
$script:HiveUnloadReady = $false
function Enable-HiveUnloadPrivileges {
    if ($script:HiveUnloadReady) { return }
    try {
        [SqiPriv.Token]::Enable('SeRestorePrivilege')
        [SqiPriv.Token]::Enable('SeBackupPrivilege')
        $script:HiveUnloadReady = $true
    } catch {
        Write-Warning "could not enable hive-unload privileges (a stranded profile hive may need a reboot to clear): $_"
    }
}

# reg.exe writes "ERROR: ..." to STDERR when a hive is already unloaded or is
# genuinely locked. Under this script's $ErrorActionPreference = 'Stop',
# Windows PowerShell 5.1 promotes ANY native command's stderr to a TERMINATING
# NativeCommandError — 2>$null does not prevent that, it only redirects where
# the text lands. Left unguarded inside Remove-TestProfile's enumeration try,
# that throw escaped the inner catch, hit the OUTER catch, and was mislabeled
# "failed to enumerate profiles" while aborting the rest of cleanup. Routing
# the unload through cmd.exe — which swallows reg's stderr with its own
# >nul 2>nul — means PowerShell sees a child that wrote nothing to stderr and
# has nothing to promote. The unload is best-effort: a hive a live process
# still holds cannot be forced and needs a reboot, which the caller reports.
function Invoke-RegUnload([string]$key) {
    & cmd.exe /c "reg unload `"$key`" >nul 2>nul"
}

function Remove-TestAccount([string]$name) {
    # Each step is independently fault-tolerant: a locked profile or a
    # pending-delete account (both happen for real on Windows) must not
    # abort cleanup of the *other* account or the work directory. Failures
    # are surfaced as warnings, never swallowed and never thrown.
    #
    # Revoking runs FIRST and only while the account still exists: the LSA
    # policy stores the grant against a SID, so deleting the account without
    # revoking would leave an unresolvable SID holding "Log on as a batch job"
    # in the local security policy of whatever machine ran this.
    try {
        if (Get-LocalUser -Name $name -ErrorAction SilentlyContinue) {
            [SqiLsaRights]::Revoke((Get-AccountSidBytes $name), 'SeBatchLogonRight')
        }
    } catch {
        Write-Warning "failed to revoke SeBatchLogonRight from '$name': $_"
    }
    try {
        if (Get-LocalUser -Name $name -ErrorAction SilentlyContinue) {
            Remove-LocalUser -Name $name -ErrorAction Stop
        }
    } catch {
        Write-Warning "failed to remove local user '$name': $_"
    }
    Remove-TestProfile $name
}

# Profile removal, which is where Windows fights back hardest. Two things the
# obvious "delete the Win32_UserProfile whose LocalPath ends in \<name>" loop
# gets wrong, both observed on a real host:
#
#  1. The directory is not always <ProfilesDirectory>\<name>. When a directory
#     for that name already exists -- a leftover from an earlier run -- Windows
#     gives the new profile "<name>.<COMPUTERNAME>" instead. Matching only
#     "*\<name>" skips it silently, so leftovers accumulate one directory per
#     run rather than being cleaned up by the next one.
#
#  2. A profile whose registry hive is still mounted cannot be deleted at all:
#     the delete fails with "being used by another process" and BOTH the record
#     and the directory survive. A hive outlives the process that loaded it --
#     it stays in HKEY_USERS until something calls UnloadUserProfile or the
#     machine reboots -- so any crash between loadProfile and Credential.Close
#     strands one. The UnloadUserProfileW panic did exactly that.
#
# So: match both spellings, unload a stranded hive and retry, then sweep any
# directory still on disk. That last step is not redundant -- an orphaned
# directory whose profile record is already gone has no Win32_UserProfile
# instance left to delete through, and would otherwise stay forever.
function Remove-TestProfile([string]$name) {
    try {
        Get-CimInstance Win32_UserProfile -ErrorAction Stop |
            Where-Object {
                $leaf = Split-Path $_.LocalPath -Leaf
                $leaf -eq $name -or $leaf -like "$name.*"
            } |
            ForEach-Object {
                # Captured because $_ inside the catch blocks below is the
                # error record, not the profile.
                $prof = $_
                try {
                    Remove-CimInstance -InputObject $prof -ErrorAction Stop
                } catch {
                    # A mounted hive blocks the delete. BOTH the main hive and
                    # the Classes hive (UsrClass.dat, mounted as <SID>_Classes,
                    # a SEPARATE key) must be unloaded -- the Classes hive is
                    # the one that actually holds the lock, so unloading only
                    # <SID> leaves the profile just as stuck. Each is a
                    # harmless no-op when nothing is mounted at that key.
                    Enable-HiveUnloadPrivileges
                    foreach ($key in @("HKU\$($prof.SID)_Classes", "HKU\$($prof.SID)")) {
                        Invoke-RegUnload $key
                    }
                    try {
                        Remove-CimInstance -InputObject $prof -ErrorAction Stop
                    } catch {
                        Write-Warning "failed to remove profile '$($prof.LocalPath)': $_"
                    }
                }
            }
    } catch {
        Write-Warning "failed to enumerate profiles for '$name': $_"
    }

    # The profile root is configurable, so read it rather than assuming
    # C:\Users.
    $root = $null
    try {
        $key = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList'
        $root = [Environment]::ExpandEnvironmentVariables((Get-ItemProperty -Path $key -ErrorAction Stop).ProfilesDirectory)
    } catch {
        Write-Warning "failed to read the profile root from the registry: $_"
    }
    if (-not $root -or -not (Test-Path -LiteralPath $root)) { return }

    Get-ChildItem -LiteralPath $root -Directory -Force -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -eq $name -or $_.Name -like "$name.*" } |
        ForEach-Object {
            $dir = $_.FullName
            try {
                Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction Stop
            } catch {
                Write-Warning ("leftover profile directory '$dir' could not be removed: " +
                    "$($_.Exception.Message.Trim()) A registry hive may still be mounted for it -- " +
                    "unload the matching HKEY_USERS key with 'reg unload', or reboot.")
            }
        }
}

$passA = New-RandomPassword
$passB = New-RandomPassword
$failed = $false

try {
    New-Item -ItemType Directory -Path $work -Force | Out-Null

    # The scratch directory ends up holding the SYSTEM-tier batch file with
    # the throwaway passwords embedded in it (see the tier-2 comment below),
    # plus the built test binary and its log. sqi-iso-a/-b are standard
    # users and must not be able to read their own or each other's
    # passwords, so lock the directory to SYSTEM and Administrators only
    # before anything is written into it. SIDs are used instead of names so
    # this does not depend on locale (Administrators is localized).
    icacls $work /inheritance:r | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls /inheritance:r failed with exit code $LASTEXITCODE" }
    icacls $work /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls /grant:r failed with exit code $LASTEXITCODE" }

    Write-Host '==> creating throwaway accounts'
    foreach ($pair in @(@($userA, $passA), @($userB, $passB))) {
        Remove-TestAccount $pair[0]
        $secure = ConvertTo-SecureString $pair[1] -AsPlainText -Force
        New-LocalUser -Name $pair[0] -Password $secure -AccountNeverExpires `
            -PasswordNeverExpires -UserMayNotChangePassword | Out-Null
        # Without this every LogonUserW in tier 2 fails with
        # ERROR_LOGON_TYPE_NOT_GRANTED -- see Grant-BatchLogonRight above.
        Grant-BatchLogonRight $pair[0]
    }

    Write-Host '==> building test binary'
    Push-Location $repo
    try {
        & go test -c -tags integration -o $binary ./test/integration/
        if ($LASTEXITCODE -ne 0) { throw "go test -c failed with $LASTEXITCODE" }
    } finally {
        Pop-Location
    }

    $envPairs = @{
        SQI_TEST_ISOLATION_WINDOWS = '1'
        SQI_TEST_ISOLATION_USER_A  = $userA
        SQI_TEST_ISOLATION_PASS_A  = $passA
        SQI_TEST_ISOLATION_USER_B  = $userB
        SQI_TEST_ISOLATION_PASS_B  = $passB
        SQI_TEST_ISOLATION_WORKDIR = $work
    }
    foreach ($k in $envPairs.Keys) { Set-Item -Path "env:$k" -Value $envPairs[$k] }

    Write-Host '==> tier 1: elevated administrator'
    # Every argument is quoted so PowerShell passes it through as a literal
    # string. Windows PowerShell 5.1 parses a BARE -test.v / -test.run as a
    # parameter token and mangles it at the '.', so the test binary receives
    # "-test" and dies with "flag provided but not defined: -test" before
    # running anything -- an empty tier that still looks like it ran. Tier 2
    # is immune only because its arguments go through a generated .cmd file
    # that cmd.exe parses instead.
    & $binary '-test.v' '-test.run' 'TestIsolationWindows_'
    if ($LASTEXITCODE -ne 0) { $failed = $true }

    Write-Host '==> tier 2: SYSTEM (scheduled task)'
    $log     = Join-Path $work 'system-tier.log'
    $code    = Join-Path $work 'system-tier.code'
    $cmdFile = Join-Path $work 'system-tier.cmd'

    # The scheduled task inherits none of this shell's environment, so the
    # variables (including the throwaway passwords) are written into a batch
    # FILE that /tr points at, rather than into an inline "cmd /c ..."
    # command line. Two independent reasons this matters:
    #
    #  1. cmd.exe percent-expands an ENTIRE logical command line in a single
    #     pass, before any of its &-chained pieces execute. An inline line
    #     of the form "... & echo %ERRORLEVEL%" therefore always echoes the
    #     errorlevel from BEFORE the line ran, not the test binary's exit
    #     code — verified empirically:
    #       > cmd /c "cd /d C:\NoSuchDir_ABC123 & echo %ERRORLEVEL%"
    #       The system cannot find the path specified.
    #       0
    #     A batch FILE parses and executes each line separately, so
    #     %ERRORLEVEL% on a later line correctly reflects the previous
    #     line's result. Do NOT collapse this back into one inline command
    #     line — it silently breaks the pass/fail signal and every later
    #     task in this project depends on this harness catching a real
    #     failure.
    #  2. Keeping the "set" lines out of the schtasks command line keeps the
    #     passwords out of `schtasks /query /tn ... /v`, the task XML under
    #     C:\Windows\System32\Tasks, and command-line auditing of the
    #     `schtasks /create` call. The directory-level ACL above keeps the
    #     throwaway standard-user accounts from reading the file directly;
    #     it is deleted with the rest of $work in the finally block.
    $cmdLines = @('@echo off')
    foreach ($k in $envPairs.Keys) { $cmdLines += "set $k=$($envPairs[$k])" }
    $cmdLines += "`"$binary`" -test.v -test.run TestIsolationWindowsSystem_ > `"$log`" 2>&1"
    $cmdLines += "echo %ERRORLEVEL% > `"$code`""
    Set-Content -Path $cmdFile -Value $cmdLines -Encoding ASCII

    # The /tr value must survive as a SINGLE argv token that contains both
    # an embedded space (a scratch path derived from a real user's full
    # name commonly contains one, even though $env:TEMP does not on this
    # machine) and embedded double quotes around the .cmd path. PowerShell's
    # native-command argument marshaling wraps a space-containing argument
    # in an outer pair of double quotes but does NOT reinterpret quote
    # characters already present in the string -- so building the value
    # with backslash-escaped quotes (\") here, rather than bare double
    # quotes, is what makes the resulting Win32 command line parse back
    # into the single intended argument. Verified empirically against a
    # path containing a space; do not "simplify" this back to bare double
    # quotes (`"$cmdFile`") -- that construction splits the value across
    # multiple argv tokens whenever the path contains a space.
    $trValue = 'cmd /c \"' + $cmdFile + '\"'
    schtasks /create /tn $taskName /tr $trValue /ru SYSTEM /rl HIGHEST /sc ONCE /st 00:00 /f | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "schtasks /create failed to register the scheduled task (exit code $LASTEXITCODE)"
    }
    schtasks /run /tn $taskName | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "schtasks /run failed to launch the scheduled task (exit code $LASTEXITCODE)"
    }

    $deadline = (Get-Date).AddMinutes(10)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 2
        if (Test-Path $code) { break }
    }

    if (-not (Test-Path $code)) {
        $state = schtasks /query /tn $taskName /v /fo list 2>$null | Out-String
        $partial = if (Test-Path $log) { Get-Content $log -Raw } else { '(no log written yet)' }
        throw "SYSTEM tier did not finish within 10 minutes`ntask state:`n$state`npartial log:`n$partial"
    }

    Get-Content $log
    $systemExit = (Get-Content $code).Trim()
    if ($systemExit -ne '0') { $failed = $true }
}
finally {
    # Each step below must survive the others failing: a wedged scheduled
    # task, a locked profile, or a pending-delete account must not leave
    # the remaining cleanup (especially the work directory holding the
    # passwords) undone.
    try {
        schtasks /delete /tn $taskName /f 2>$null | Out-Null
    } catch {
        Write-Warning "failed to delete scheduled task '$taskName': $_"
    }
    Remove-TestAccount $userA
    Remove-TestAccount $userB
    try {
        if (Test-Path $work) { Remove-Item -Recurse -Force $work -ErrorAction Stop }
    } catch {
        Write-Warning "failed to remove work directory '$work': $_"
    }
}

if ($failed) { exit 1 }
Write-Host '==> windows isolation suite passed'
