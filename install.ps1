$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Repo = 'digitaldrywood/detent'
$ProjectName = 'detent'
$ModulePackage = 'github.com/digitaldrywood/detent/cmd/detent'
$Headers = @{ 'User-Agent' = 'detent-installer' }
$ApiBase = if ($env:DETENT_GITHUB_API_BASE) { $env:DETENT_GITHUB_API_BASE } else { "https://api.github.com/repos/$Repo" }
$DownloadBase = if ($env:DETENT_RELEASE_DOWNLOAD_BASE) { $env:DETENT_RELEASE_DOWNLOAD_BASE } else { "https://github.com/$Repo/releases/download" }
$InstallMode = if ($env:DETENT_INSTALL_MODE) { $env:DETENT_INSTALL_MODE } else { 'auto' }
$StateDir = if ($env:DETENT_STATE_DIR) {
	$env:DETENT_STATE_DIR
} elseif ($env:LOCALAPPDATA) {
	Join-Path $env:LOCALAPPDATA 'detent'
} else {
	Join-Path $HOME '.detent'
}
$InstallLock = if ($env:DETENT_INSTALL_LOCK) { $env:DETENT_INSTALL_LOCK } else { Join-Path $StateDir 'install.lock' }

try {
	[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {
}

function Abort {
	param([string]$Message)
	Write-Error $Message
	exit 1
}

function Test-Command {
	param([string]$Name)
	return [bool](Get-Command $Name -ErrorAction SilentlyContinue)
}

function Join-Url {
	param(
		[string]$Base,
		[string]$Path
	)
	return "$($Base.TrimEnd('/'))/$($Path.TrimStart('/'))"
}

function Remove-LeadingV {
	param([string]$Version)
	if ($Version.StartsWith('v')) {
		return $Version.Substring(1)
	}
	return $Version
}

function Get-InstallDir {
	if ($env:DETENT_INSTALL_DIR) {
		return $env:DETENT_INSTALL_DIR
	}
	if ($env:LOCALAPPDATA) {
		return Join-Path $env:LOCALAPPDATA 'detent\bin'
	}
	return Join-Path $HOME '.detent\bin'
}

function Convert-ToTargetArch {
	param([string]$Arch)

	if ([string]::IsNullOrWhiteSpace($Arch)) {
		return $null
	}

	switch -Wildcard ($Arch.Trim().ToLowerInvariant()) {
		'x64*' { return 'amd64' }
		'amd64*' { return 'amd64' }
		'x86_64*' { return 'amd64' }
		'arm64*' { return 'arm64' }
		'aarch64*' { return 'arm64' }
	}

	return $null
}

function Select-TargetArch {
	param([object[]]$Candidates)

	foreach ($arch in $Candidates) {
		$targetArch = Convert-ToTargetArch $arch
		if ($targetArch) {
			return $targetArch
		}
	}

	return $null
}

function Convert-CimProcessorArchitectureToTargetArch {
	param($Architecture)

	switch ([string]$Architecture) {
		'9' { return 'amd64' }
		'12' { return 'arm64' }
	}

	return $null
}

function Convert-CimOSArchitectureToTargetArch {
	param([string]$Architecture)

	if ([string]::IsNullOrWhiteSpace($Architecture)) {
		return $null
	}

	$normalized = $Architecture.Trim().ToLowerInvariant()
	if ($normalized -eq '64-bit') {
		return 'amd64'
	}

	return Convert-ToTargetArch $Architecture
}

function Get-ProcessorArchitectureCandidate {
	$testArch = [Environment]::GetEnvironmentVariable('DETENT_INSTALL_TEST_PROCESSOR_ARCHITECTURE', 'Process')
	if ($null -ne $testArch) {
		return $testArch
	}
	return $env:PROCESSOR_ARCHITECTURE
}

function Get-ProcessorArchitectureW6432Candidate {
	$testArch = [Environment]::GetEnvironmentVariable('DETENT_INSTALL_TEST_PROCESSOR_ARCHITEW6432', 'Process')
	if ($null -ne $testArch) {
		return $testArch
	}
	return $env:PROCESSOR_ARCHITEW6432
}

function Get-OSArchitectureCandidates {
	$candidates = @()
	if ($env:DETENT_INSTALL_TEST_OS_ARCH) {
		$candidates += $env:DETENT_INSTALL_TEST_OS_ARCH
	} elseif ($env:DETENT_INSTALL_TEST_OS_ARCH_UNAVAILABLE -ne '1') {
		try {
			$candidates += [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
		} catch {
		}
	}

	$candidates += Get-ProcessorArchitectureW6432Candidate

	try {
		$testCimProcessorArch = [Environment]::GetEnvironmentVariable('DETENT_INSTALL_TEST_CIM_PROCESSOR_ARCH', 'Process')
		$testCimOSArch = [Environment]::GetEnvironmentVariable('DETENT_INSTALL_TEST_CIM_OS_ARCH', 'Process')
		if (-not [string]::IsNullOrEmpty($testCimProcessorArch) -or -not [string]::IsNullOrEmpty($testCimOSArch)) {
			$candidates += Convert-CimProcessorArchitectureToTargetArch $testCimProcessorArch
			$candidates += Get-ProcessorArchitectureCandidate
			$candidates += Convert-CimOSArchitectureToTargetArch $testCimOSArch
		} elseif (Test-Command 'Get-CimInstance') {
			$processor = Get-CimInstance -ClassName Win32_Processor -ErrorAction Stop | Select-Object -First 1
			$candidates += Convert-CimProcessorArchitectureToTargetArch $processor.Architecture
			$candidates += Get-ProcessorArchitectureCandidate

			$os = Get-CimInstance -ClassName Win32_OperatingSystem -ErrorAction Stop
			$candidates += Convert-CimOSArchitectureToTargetArch $os.OSArchitecture
		}
	} catch {
	}

	return $candidates
}

function Get-ProcessArchitectureCandidates {
	$candidates = @()
	if ($env:DETENT_INSTALL_TEST_PROCESS_ARCH) {
		$candidates += $env:DETENT_INSTALL_TEST_PROCESS_ARCH
	} else {
		try {
			$candidates += [System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString()
		} catch {
		}
	}
	$candidates += Get-ProcessorArchitectureCandidate

	return $candidates
}

function Get-TargetArch {
	if ($env:DETENT_INSTALL_TEST_ARCH) {
		return $env:DETENT_INSTALL_TEST_ARCH
	}

	$targetArch = Select-TargetArch (Get-OSArchitectureCandidates)
	if ($targetArch) {
		return $targetArch
	}

	return Select-TargetArch (Get-ProcessArchitectureCandidates)
}

function Save-Url {
	param(
		[string]$Url,
		[string]$Path
	)
	Invoke-WebRequest -Uri $Url -OutFile $Path -Headers $Headers -UseBasicParsing | Out-Null
}

function Save-OptionalUrl {
	param(
		[string]$Url,
		[string]$Path
	)
	try {
		Save-Url $Url $Path
		return $true
	} catch {
		return $false
	}
}

function Get-ReleaseTag {
	if ($env:DETENT_VERSION) {
		return $env:DETENT_VERSION
	}

	try {
		$release = Invoke-RestMethod -Uri (Join-Url $ApiBase 'releases/latest') -Headers $Headers
		if ($release.tag_name) {
			return [string]$release.tag_name
		}
	} catch {
	}
	return $null
}

function Save-ReleaseArchive {
	param(
		[string]$Tag,
		[string]$Version,
		[string]$Arch,
		[string]$Output
	)

	$versionWithoutV = Remove-LeadingV $Version
	$assetNames = @("${ProjectName}_${versionWithoutV}_windows_${Arch}.zip")
	if ($Version -ne $versionWithoutV) {
		$assetNames += "${ProjectName}_${Version}_windows_${Arch}.zip"
	}

	foreach ($assetName in $assetNames) {
		if (Save-OptionalUrl (Join-Url $DownloadBase "$Tag/$assetName") $Output) {
			return $assetName
		}
	}
	return $null
}

function Save-Checksums {
	param(
		[string]$Tag,
		[string]$Version,
		[string]$Output
	)

	$versionWithoutV = Remove-LeadingV $Version
	$checksumNames = @("${ProjectName}_${versionWithoutV}_checksums.txt", 'checksums.txt')
	foreach ($checksumName in $checksumNames) {
		if (Save-OptionalUrl (Join-Url $DownloadBase "$Tag/$checksumName") $Output) {
			return $true
		}
	}
	return $false
}

function Get-ExpectedChecksum {
	param(
		[string]$Checksums,
		[string]$AssetName
	)

	foreach ($line in Get-Content -LiteralPath $Checksums) {
		$parts = $line.Trim() -split '\s+', 2
		if ($parts.Count -ne 2) {
			continue
		}
		$file = $parts[1].TrimStart([char]'*')
		if ($file -eq $AssetName) {
			return $parts[0].ToLowerInvariant()
		}
	}
	return $null
}

function Get-Sha256File {
	param([string]$Path)

	$stream = [IO.File]::OpenRead($Path)
	try {
		$sha = [Security.Cryptography.SHA256]::Create()
		try {
			$hash = $sha.ComputeHash($stream)
		} finally {
			$sha.Dispose()
		}
	} finally {
		$stream.Dispose()
	}

	return -join ($hash | ForEach-Object { $_.ToString('x2') })
}

function Assert-Checksum {
	param(
		[string]$Archive,
		[string]$Checksums,
		[string]$AssetName
	)

	$expected = Get-ExpectedChecksum $Checksums $AssetName
	if (-not $expected) {
		Abort "Checksum for $AssetName not found"
	}

	$actual = Get-Sha256File $Archive
	if ($actual -ne $expected) {
		Abort "Checksum mismatch for ${AssetName}: expected $expected, got $actual"
	}
	Write-Host "Verified checksum for $AssetName"
}

function Install-Release {
	param([string]$Arch)

	$tag = Get-ReleaseTag
	if (-not $tag) {
		Write-Warning 'Could not resolve the latest Detent release; falling back to go install'
		return $false
	}

	$archive = Join-Path $TmpDir 'archive.zip'
	$checksums = Join-Path $TmpDir 'checksums.txt'
	$assetName = Save-ReleaseArchive $tag $tag $Arch $archive
	if (-not $assetName) {
		Write-Warning "No Detent release asset found for $tag windows/$Arch; falling back to go install"
		return $false
	}

	if (-not (Save-Checksums $tag $tag $checksums)) {
		Abort "Could not download checksums for release $tag"
	}
	Assert-Checksum $archive $checksums $assetName

	$releaseDir = Join-Path $TmpDir 'release'
	New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null
	Expand-Archive -Path $archive -DestinationPath $releaseDir -Force

	$binary = Join-Path $releaseDir 'detent.exe'
	if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
		$candidate = Get-ChildItem -LiteralPath $releaseDir -Filter 'detent.exe' -Recurse -File | Select-Object -First 1
		if (-not $candidate) {
			Abort "Release archive $assetName did not contain detent.exe"
		}
		$binary = $candidate.FullName
	}

	Copy-Item -LiteralPath $binary -Destination (Join-Path $TmpDir 'detent.exe') -Force
	return $true
}

function Install-Go {
	$version = if ($env:DETENT_VERSION) { $env:DETENT_VERSION } else { 'latest' }
	if (-not (Test-Command 'go')) {
		Abort 'Cannot install Detent: release asset unavailable and go is not installed'
	}

	$goBin = Join-Path $TmpDir 'go-bin'
	New-Item -ItemType Directory -Force -Path $goBin | Out-Null

	$previousGoBin = $env:GOBIN
	try {
		$env:GOBIN = $goBin
		& go install "$ModulePackage@$version"
		if ($LASTEXITCODE -ne 0) {
			Abort 'go install failed'
		}
	} finally {
		$env:GOBIN = $previousGoBin
	}

	$binary = Join-Path $goBin 'detent.exe'
	if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
		Abort 'go install did not produce detent.exe'
	}
	Copy-Item -LiteralPath $binary -Destination (Join-Path $TmpDir 'detent.exe') -Force
}

function Install-ReleaseOrGo {
	if ($TargetArch) {
		if (Install-Release $TargetArch) {
			return
		}
	} else {
		Write-Warning 'No supported Windows release target detected; falling back to go install'
	}
	Install-Go
}

function Split-PathList {
	param([string]$Value)
	if ([string]::IsNullOrWhiteSpace($Value)) {
		return @()
	}
	return $Value -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
}

function Test-PathListContains {
	param(
		[string]$Value,
		[string]$Dir
	)

	$target = [IO.Path]::GetFullPath($Dir).TrimEnd('\')
	foreach ($entry in Split-PathList $Value) {
		try {
			$expanded = [Environment]::ExpandEnvironmentVariables($entry)
			$normalized = [IO.Path]::GetFullPath($expanded).TrimEnd('\')
		} catch {
			$normalized = $entry.TrimEnd('\')
		}
		if ([string]::Equals($normalized, $target, [StringComparison]::OrdinalIgnoreCase)) {
			return $true
		}
	}
	return $false
}

function Add-ToUserPath {
	param([string]$Dir)

	if ($env:DETENT_INSTALL_SKIP_PATH -eq '1') {
		return $false
	}

	$fullDir = [IO.Path]::GetFullPath($Dir)
	if (-not (Test-PathListContains $env:Path $fullDir)) {
		$env:Path = "$fullDir;$env:Path"
	}

	$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
	if (Test-PathListContains $userPath $fullDir) {
		return $false
	}

	$newUserPath = if ([string]::IsNullOrWhiteSpace($userPath)) { $fullDir } else { "$fullDir;$userPath" }
	[Environment]::SetEnvironmentVariable('Path', $newUserPath, 'User')
	return $true
}

$InstallDir = Get-InstallDir
$Target = Join-Path $InstallDir 'detent.exe'
$TargetArch = Get-TargetArch
$TmpDir = Join-Path ([IO.Path]::GetTempPath()) "detent-install-$([Guid]::NewGuid().ToString('N'))"

if ($TargetArch) {
	Write-Host "Detected target windows/$TargetArch"
}

New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null
try {
	switch ($InstallMode) {
		'auto' { Install-ReleaseOrGo; break }
		'release' { Install-ReleaseOrGo; break }
		'go' { Install-Go; break }
		default { Abort "Unknown DETENT_INSTALL_MODE: $InstallMode" }
	}

	New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
	New-Item -ItemType Directory -Force -Path $StateDir | Out-Null
	Copy-Item -LiteralPath (Join-Path $TmpDir 'detent.exe') -Destination $Target -Force
	@(
		"binary=$Target"
		"installed_at=$((Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ'))"
	) | Set-Content -LiteralPath $InstallLock -Encoding utf8
	$pathChanged = Add-ToUserPath $InstallDir

	Write-Host "Installed Detent at $Target"
	if ($pathChanged) {
		Write-Host "Added $InstallDir to the user PATH. Open a new terminal before running detent."
	}
} finally {
	Remove-Item -LiteralPath $TmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
