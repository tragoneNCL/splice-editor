# Install Splice on Windows (PowerShell):
#
#   irm https://raw.githubusercontent.com/tragoneNCL/splice-editor/main/install.ps1 | iex
#
# Downloads the latest release, verifies its SHA-256 checksum, installs under
# %LOCALAPPDATA%\Splice, adds a Start menu shortcut, and installs ffmpeg with
# winget if it is missing. No administrator rights needed.
$ErrorActionPreference = "Stop"
$repo = if ($env:SPLICE_REPO) { $env:SPLICE_REPO } else { "tragoneNCL/splice-editor" }
$api  = if ($env:SPLICE_API_BASE) { $env:SPLICE_API_BASE } else { "https://api.github.com" }
$dest = if ($env:SPLICE_DEST) { $env:SPLICE_DEST } else { Join-Path $env:LOCALAPPDATA "Splice" }

Write-Host "Looking up the latest Splice release..."
$rel = Invoke-RestMethod -Uri "$api/repos/$repo/releases/latest" -Headers @{ Accept = "application/vnd.github+json" }
$tag = $rel.tag_name
$assetName = "splice-$tag-windows-amd64.zip"
$asset = $rel.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
$sums  = $rel.assets | Where-Object { $_.name -eq "checksums.txt" } | Select-Object -First 1
if (-not $asset) { throw "release $tag has no Windows build" }
if (-not $sums)  { throw "release $tag has no checksums.txt; refusing to install unverified code" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("splice-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  $zip = Join-Path $tmp $assetName
  Write-Host "Downloading $assetName ..."
  Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zip
  $sumFile = Join-Path $tmp "checksums.txt"
  Invoke-WebRequest -Uri $sums.browser_download_url -OutFile $sumFile

  $line = Get-Content $sumFile | Where-Object { $_ -match [regex]::Escape($assetName) } | Select-Object -First 1
  if (-not $line) { throw "no checksum listed for $assetName" }
  $want = ($line -split "\s+")[0].ToLower()
  $got  = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
  if ($want -ne $got) { throw "checksum mismatch; not installing" }
  Write-Host "Checksum verified."

  Expand-Archive -Path $zip -DestinationPath $tmp -Force
  $exe = Get-ChildItem -Path $tmp -Recurse -Filter "splice.exe" | Select-Object -First 1
  if (-not $exe) { throw "the archive did not contain the app" }
  New-Item -ItemType Directory -Path $dest -Force | Out-Null
  Copy-Item $exe.FullName (Join-Path $dest "splice.exe") -Force
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# Start menu shortcut that opens the app in the browser.
$lnk = Join-Path ([Environment]::GetFolderPath("Programs")) "Splice.lnk"
$sh = New-Object -ComObject WScript.Shell
$s = $sh.CreateShortcut($lnk)
$s.TargetPath = Join-Path $dest "splice.exe"
$s.Arguments = "-open"
$s.WorkingDirectory = $dest
$s.Save()

if (-not (Get-Command ffmpeg -ErrorAction SilentlyContinue)) {
  Write-Host "ffmpeg is needed to process video; installing it with winget..."
  try { winget install --id Gyan.FFmpeg -e --accept-source-agreements --accept-package-agreements }
  catch { Write-Host "Please install ffmpeg (winget install Gyan.FFmpeg), then start Splice." }
}

Write-Host ""
Write-Host "Installed Splice $tag -> $dest\splice.exe"
Write-Host "Open it from the Start menu (Splice). It updates itself; you only install once."
