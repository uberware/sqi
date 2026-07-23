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

function Remove-TestAccount([string]$name) {
    # Each step is independently fault-tolerant: a locked profile or a
    # pending-delete account (both happen for real on Windows) must not
    # abort cleanup of the *other* account or the work directory. Failures
    # are surfaced as warnings, never swallowed and never thrown.
    try {
        if (Get-LocalUser -Name $name -ErrorAction SilentlyContinue) {
            Remove-LocalUser -Name $name -ErrorAction Stop
        }
    } catch {
        Write-Warning "failed to remove local user '$name': $_"
    }
    try {
        Get-CimInstance Win32_UserProfile -ErrorAction Stop |
            Where-Object { $_.LocalPath -like "*\$name" } |
            ForEach-Object {
                try {
                    Remove-CimInstance -InputObject $_ -ErrorAction Stop
                } catch {
                    Write-Warning "failed to remove profile for '$name': $_"
                }
            }
    } catch {
        Write-Warning "failed to enumerate profiles for '$name': $_"
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
    & $binary -test.v -test.run 'TestIsolationWindows_'
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
