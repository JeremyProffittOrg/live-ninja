# Create an Entra client secret for ln-voicelive-client, store it as GitHub
# Actions secrets (and gitignored .env). Prints length + sha256 fingerprint
# only. Never prints the secret.
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot
$repo = 'JeremyProffittOrg/live-ninja'
$appId = 'be6bce90-e5e8-469b-b84a-745266717ca1'
$openssl = 'C:\Program Files\Git\usr\bin\openssl.exe'
$work = Join-Path $env:TEMP 'ln-voicelive-secret'
New-Item -ItemType Directory -Force -Path $work | Out-Null
$secretFile = Join-Path $work 'client-secret.txt'
$idFile = Join-Path $work 'client-id.txt'
try {
  cmd /c "az ad app credential reset --id $appId --append --display-name live-ninja-a6 --years 2 --query password --output tsv 1>`"$secretFile`" 2>nul"
  if ($LASTEXITCODE -ne 0) { throw "az ad app credential reset failed: $LASTEXITCODE" }
  if (-not (Test-Path $secretFile) -or (Get-Item $secretFile).Length -lt 8) {
    throw "az did not write a client secret"
  }
  $secretRaw = [System.IO.File]::ReadAllText($secretFile).Trim()
  if ($secretRaw.Length -lt 8) { throw "client secret too short" }
  [System.IO.File]::WriteAllText($secretFile, $secretRaw)
  [System.IO.File]::WriteAllText($idFile, $appId)

  Get-Content -Raw -LiteralPath $secretFile | gh secret set AZURE_VOICELIVE_CLIENT_SECRET -R $repo
  if ($LASTEXITCODE -ne 0) { throw "gh secret set AZURE_VOICELIVE_CLIENT_SECRET failed: $LASTEXITCODE" }
  Get-Content -Raw -LiteralPath $idFile | gh secret set AZURE_VOICELIVE_CLIENT_ID -R $repo
  if ($LASTEXITCODE -ne 0) { throw "gh secret set AZURE_VOICELIVE_CLIENT_ID failed: $LASTEXITCODE" }

  $fp = (& $openssl dgst -sha256 $secretFile | Select-Object -Last 1).ToString().Split(' ')[-1]
  $len = $secretRaw.Length

  $envPath = Join-Path $repoRoot '.env'
  $block = @"
# Azure Voice Live client (do not commit; .gitignore includes .env)
AZURE_VOICELIVE_CLIENT_ID=$appId
AZURE_VOICELIVE_CLIENT_SECRET=$secretRaw
"@
  if (Test-Path $envPath) {
    $existing = [System.IO.File]::ReadAllText($envPath)
    $existing = [regex]::Replace($existing, '(?ms)^# Azure Voice Live client.*?(?=^# |\z)', '')
    $existing = [regex]::Replace($existing, '(?m)^AZURE_VOICELIVE_CLIENT_(ID|SECRET)=.*\r?\n?', '')
    $out = $existing.TrimEnd() + "`r`n`r`n" + $block
  } else {
    $out = $block
  }
  [System.IO.File]::WriteAllText($envPath, $out.Trim() + "`r`n")

  Write-Host "OK: GitHub secret AZURE_VOICELIVE_CLIENT_SECRET (len $len, sha256:$($fp.Substring(0,12))...)"
  Write-Host "OK: GitHub secret AZURE_VOICELIVE_CLIENT_ID ($appId)"
  Write-Host "OK: wrote $envPath (gitignored)"
} finally {
  $secretRaw = $null
  Remove-Item -Force -ErrorAction SilentlyContinue $secretFile, $idFile
  Remove-Item -Force -Recurse -ErrorAction SilentlyContinue $work
}
