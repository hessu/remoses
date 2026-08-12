<#
.SYNOPSIS
    Installs remoses on Windows.

.DESCRIPTION
    Works out which build this machine needs, downloads it from the GitHub
    release, checks it against the published SHA256SUMS, and puts two
    executables somewhere on your PATH. Nothing else is installed, nothing is
    left running, and no administrator rights are needed: it installs for the
    current user.

    The one-liner:

        irm https://raw.githubusercontent.com/hessu/remoses/main/install.ps1 | iex

    With options, which needs the slightly awkward form because a piped script
    has nowhere to put arguments:

        & ([scriptblock]::Create((irm https://raw.githubusercontent.com/hessu/remoses/main/install.ps1))) -Version v0.1.0

    To read it first — the right instinct, and unfortunately the fiddlier path:

        irm https://raw.githubusercontent.com/hessu/remoses/main/install.ps1 -OutFile install.ps1
        notepad install.ps1
        powershell -ExecutionPolicy Bypass -File .\install.ps1

    The -ExecutionPolicy flag is needed on that last one and not on the
    one-liner, which is exactly backwards from how it ought to feel. Execution
    policy governs script *files*, not commands piped into a session;
    Microsoft's own documentation says it "isn't a security system that
    restricts user actions". The checksum below is the part actually protecting
    you, and it runs either way.

.PARAMETER Version
    Release tag to install. Defaults to the latest.

.PARAMETER Dir
    Where to install. Defaults to %LOCALAPPDATA%\Programs\remoses.

.PARAMETER NoPath
    Do not add the install directory to your PATH.
#>
param(
    [string] $Version = '',
    [string] $Dir = "$env:LOCALAPPDATA\Programs\remoses",
    [switch] $NoPath
)

# Stop on the first error, so a failed download cannot fall through into
# installing whatever happens to be in the temporary directory.
#
# Deliberately no Set-StrictMode and no [CmdletBinding()]. Both are good
# practice in a module and both add ways for this to fail in an environment
# nobody here can test: strict mode throws on a property a JSON response
# happens not to carry, and CmdletBinding adds parameter machinery to a script
# that is usually run by being piped into Invoke-Expression.
$ErrorActionPreference = 'Stop'

$Repo = if ($env:REMOSES_REPO) { $env:REMOSES_REPO } else { 'hessu/remoses' }
$BaseUrl = if ($env:REMOSES_BASE_URL) { $env:REMOSES_BASE_URL } else { "https://github.com/$Repo/releases" }
$ApiUrl = if ($env:REMOSES_API_URL) { $env:REMOSES_API_URL } else { "https://api.github.com/repos/$Repo/releases/latest" }

# Windows PowerShell 5.1 on an unpatched machine may still default to TLS 1.0,
# which GitHub refuses. Harmless where the default is already sane.
try {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {
    # PowerShell 7 on .NET 5+ manages this itself and the type may be absent.
}

function Get-RemosesPlatform {
    # PROCESSOR_ARCHITECTURE reports the architecture of the *process*, so a
    # 32-bit PowerShell on a 64-bit machine says x86. PROCESSOR_ARCHITEW6432 is
    # set only in that case and names the real machine, which is why both are
    # consulted rather than either alone.
    $arch = $env:PROCESSOR_ARCHITECTURE
    if ($env:PROCESSOR_ARCHITEW6432) { $arch = $env:PROCESSOR_ARCHITEW6432 }

    switch ($arch) {
        'AMD64' { return 'windows-amd64' }
        'ARM64' { return 'windows-arm64' }
        'x86' {
            throw "remoses has no 32-bit x86 build. If this is a 64-bit machine, run this from a 64-bit PowerShell."
        }
        default {
            throw "no release build for processor architecture '$arch' — see $BaseUrl"
        }
    }
}

function Get-LatestVersion {
    Write-Host 'looking up the latest release...'
    # Invoke-RestMethod parses the JSON, so this needs no third-party module.
    $release = Invoke-RestMethod -Uri $ApiUrl -UseBasicParsing
    $tag = $null
    if ($release -and ($release.PSObject.Properties.Name -contains 'tag_name')) {
        $tag = $release.tag_name
    }
    if (-not $tag) {
        throw "could not work out the latest release; pass -Version"
    }
    return $tag
}

$platform = Get-RemosesPlatform
if (-not $Version) { $Version = Get-LatestVersion }

$archive = "remoses-$Version-$platform.zip"
$url = "$BaseUrl/download/$Version/$archive"
$sumsUrl = "$BaseUrl/download/$Version/SHA256SUMS"

Write-Host "remoses $Version for $platform"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("remoses-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    $zip = Join-Path $tmp $archive
    Write-Host "downloading $archive"
    try {
        Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing
    } catch {
        throw "could not download $url`nIf that release has no build for $platform, the list is at $BaseUrl"
    }

    # Verified before unpacking. A checksum checked afterwards is decoration.
    Write-Host 'checking the checksum'
    $sums = (Invoke-WebRequest -Uri $sumsUrl -UseBasicParsing).Content
    $want = $null
    foreach ($line in ($sums -split "`n")) {
        $fields = ($line.Trim() -split '\s+')
        if ($fields.Count -ge 2 -and ($fields[1].TrimStart('*')) -eq $archive) {
            $want = $fields[0].ToLower()
            break
        }
    }
    if (-not $want) { throw "SHA256SUMS does not list $archive" }

    $got = (Get-FileHash -Path $zip -Algorithm SHA256).Hash.ToLower()
    if ($want -ne $got) {
        throw "CHECKSUM MISMATCH for $archive`n  expected $want`n  got      $got`nDo not use this download."
    }
    Write-Host '  ok'

    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $src = Join-Path $tmp "remoses-$Version-$platform"
    if (-not (Test-Path (Join-Path $src 'remoses.exe'))) {
        throw "the archive does not contain remoses.exe"
    }

    New-Item -ItemType Directory -Path $Dir -Force | Out-Null
    foreach ($exe in @('remoses.exe', 'remoses-cli.exe')) {
        Copy-Item -Path (Join-Path $src $exe) -Destination (Join-Path $Dir $exe) -Force
    }
    # The example configuration and the guide go alongside, because a fresh
    # install cannot get anywhere without the first one.
    Copy-Item -Path (Join-Path $src 'remoses.example.yaml') -Destination $Dir -Force
    Copy-Item -Path (Join-Path $src 'docs') -Destination $Dir -Recurse -Force
    Write-Host "installed $Dir\remoses.exe and $Dir\remoses-cli.exe"

    if (-not $NoPath) {
        # The user PATH, so this needs no administrator and affects nobody else.
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (-not $userPath) { $userPath = '' }
        $already = ($userPath -split ';') | Where-Object { $_.TrimEnd('\') -eq $Dir.TrimEnd('\') }
        if ($already) {
            Write-Host "$Dir is already on your PATH"
        } else {
            $new = if ($userPath.TrimEnd(';')) { $userPath.TrimEnd(';') + ';' + $Dir } else { $Dir }
            [Environment]::SetEnvironmentVariable('Path', $new, 'User')
            $env:Path = $env:Path + ';' + $Dir
            Write-Host "added $Dir to your PATH — open a new terminal for it to take effect elsewhere"
        }
    }
} finally {
    Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ''
Write-Host "done. remoses $Version is installed."
Write-Host ''
Write-Host '  remoses -version'
Write-Host '  remoses passwd                       generate a password hash for the config'
Write-Host '  remoses -config remoses.yaml -check  validate a configuration'
Write-Host '  remoses test-run                     exercise your radio and write a report'
Write-Host ''
Write-Host "An annotated example configuration is at $Dir\remoses.example.yaml,"
Write-Host "and the user guide is in $Dir\docs."
Write-Host ''
Write-Host 'On Windows a radio is usually a COM port — put that in port.device,'
Write-Host 'for example COM7. Device Manager lists them under Ports (COM & LPT).'
