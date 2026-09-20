# Generate the IoT custom-authorizer RSA keypair, store the private half as a
# GitHub Actions secret and the public half as a GitHub variable, and write
# both into a gitignored .env. Prints fingerprints only. Never prints PEM.
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot
$repo = 'JeremyProffittOrg/live-ninja'
$work = Join-Path $env:TEMP 'ln-iot-authorizer-key'
New-Item -ItemType Directory -Force -Path $work | Out-Null
$priv = Join-Path $work 'iot-authorizer-private.pem'
$pub = Join-Path $work 'iot-authorizer-public.pem'
$openssl = 'C:\Program Files\Git\usr\bin\openssl.exe'
if (-not (Test-Path $openssl)) { throw "openssl not found at $openssl" }
try {
  cmd /c "`"$openssl`" genrsa -out `"$priv`" 2048 >nul 2>&1"
  if ($LASTEXITCODE -ne 0) { throw "openssl genrsa failed: $LASTEXITCODE" }
  cmd /c "`"$openssl`" rsa -in `"$priv`" -pubout -out `"$pub`" >nul 2>&1"
  if ($LASTEXITCODE -ne 0) { throw "openssl rsa -pubout failed: $LASTEXITCODE" }
  $privFp = (& $openssl dgst -sha256 $priv | Select-Object -Last 1).ToString().Split(' ')[-1]
  $pubFp = (& $openssl dgst -sha256 $pub | Select-Object -Last 1).ToString().Split(' ')[-1]
  $privLen = (Get-Item $priv).Length
  $pubLen = (Get-Item $pub).Length
  Get-Content -Raw -LiteralPath $priv | gh secret set IOT_AUTHORIZER_SIGNING_PRIVATE_KEY -R $repo
  if ($LASTEXITCODE -ne 0) { throw "gh secret set failed: $LASTEXITCODE" }
  Get-Content -Raw -LiteralPath $pub | gh variable set IOT_AUTHORIZER_SIGNING_PUBLIC_KEY -R $repo
  if ($LASTEXITCODE -ne 0) { throw "gh variable set failed: $LASTEXITCODE" }
  $privB64 = [Convert]::ToBase64String([System.IO.File]::ReadAllBytes($priv))
  $pubB64 = [Convert]::ToBase64String([System.IO.File]::ReadAllBytes($pub))
  $envPath = Join-Path $repoRoot '.env'
  $block = @"
# IoT custom authorizer signing (do not commit; .gitignore includes .env)
# Values are base64 of the PEM files. Decode before use.
IOT_AUTHORIZER_SIGNING_PRIVATE_KEY_B64=$privB64
IOT_AUTHORIZER_SIGNING_PUBLIC_KEY_B64=$pubB64
"@
  if (Test-Path $envPath) {
    $existing = [System.IO.File]::ReadAllText($envPath)
    $existing = [regex]::Replace($existing, '(?ms)^# IoT custom authorizer.*?(?=^[A-Z_]+=|\z)', '')
    $existing = [regex]::Replace($existing, '(?m)^IOT_AUTHORIZER_SIGNING_(PRIVATE|PUBLIC)_KEY=.*\r?\n?', '')
    $out = $existing.TrimEnd() + "`r`n`r`n" + $block
  } else {
    $out = $block
  }
  [System.IO.File]::WriteAllText($envPath, $out.Trim() + "`r`n")
  Write-Host "OK: GitHub secret IOT_AUTHORIZER_SIGNING_PRIVATE_KEY (len $privLen, sha256:$($privFp.Substring(0,12))...)"
  Write-Host "OK: GitHub variable IOT_AUTHORIZER_SIGNING_PUBLIC_KEY (len $pubLen, sha256:$($pubFp.Substring(0,12))...)"
  Write-Host "OK: wrote $envPath (gitignored)"
} finally {
  Remove-Item -Force -ErrorAction SilentlyContinue $priv, $pub
  Remove-Item -Force -Recurse -ErrorAction SilentlyContinue $work
}
