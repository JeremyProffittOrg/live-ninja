# Offline regression checks. Uses synthetic values; never calls GitHub.
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/setup-phone-credentials.ps1"
# Exercise the real hidden process/stdin path before replacing it with fakes.
$echoCode = '$text = [Console]::In.ReadToEnd(); [Console]::Out.Write([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($text)))'
$echoEncoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($echoCode))
$sample = 'fake-!%&|<>^"''`$()-1234'
$roundtrip = Invoke-PhoneCli (Get-Command powershell.exe).Source @('-NoProfile','-NonInteractive','-EncodedCommand',$echoEncoded) $sample
if ($roundtrip.Code -ne 0 -or $roundtrip.Output -cne [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($sample))) {
    throw 'Hidden process stdin changed the synthetic value.'
}
$script:PhoneGh = 'unused-gh.exe'
$script:Calls = @()
$script:Messages = @()
$script:InputValue = ''
$script:FailWrite = $false
$script:FailVerify = $false
$script:PromptCount = 0
$script:VerifyName = 'TWILIO_AUTH_TOKEN'

function Write-Host { param([object]$Object); $script:Messages += [string]$Object }
function Read-Host {
    param([string]$Prompt, [switch]$AsSecureString)
    $script:PromptCount++
    $value = New-Object Security.SecureString
    foreach ($char in $script:InputValue.ToCharArray()) { $value.AppendChar($char) }
    return $value
}
function Invoke-PhoneCli {
    param([string]$Exe, [string[]]$CliArgs, [string]$InputText = '')
    $script:Calls += [pscustomobject]@{ Args=$CliArgs; Input=$InputText }
    if ($CliArgs[1] -eq 'set') {
        return [pscustomobject]@{ Code=[int]$script:FailWrite; Output='' }
    }
    return [pscustomobject]@{ Code=[int]$script:FailVerify; Output=('[{"name":"' + $script:VerifyName + '"}]') }
}
function Assert-Phone { param([bool]$OK, [string]$Message); if (-not $OK) { throw $Message } }
function Reset-PhoneTest {
    $script:Calls=@(); $script:Messages=@(); $script:PromptCount=0
    $script:FailWrite=$false; $script:FailVerify=$false
    $script:VerifyName='TWILIO_AUTH_TOKEN'
}

Reset-PhoneTest
$script:InputValue = ''
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $false) -eq 'skipped') 'Empty input did not skip.'
Assert-Phone ($script:Calls.Count -eq 0) 'Empty input wrote to GitHub.'

Reset-PhoneTest
$script:InputValue = 'must-not-be-requested'
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $true) -eq 'skipped') 'Existing value did not skip.'
Assert-Phone ($script:PromptCount -eq 0 -and $script:Calls.Count -eq 0) 'Existing value prompted or wrote.'

Reset-PhoneTest
$script:InputValue = 'synthetic-!%&|<>^"''`$()\-ABCD'
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $false) -eq 'written') 'Valid value did not write.'
Assert-Phone ($script:Calls.Count -eq 2) 'Expected one write and one names-only verification.'
Assert-Phone ($script:Calls[0].Input -ceq $script:InputValue) 'Input was altered.'
Assert-Phone (($script:Calls[0].Args -join ' ') -eq 'secret set TWILIO_AUTH_TOKEN --repo JeremyProffittOrg/live-ninja') 'Wrong scope or value in arguments.'
Assert-Phone (($script:Messages -join '|').Contains('last four: ABCD')) 'Missing requested suffix.'
Assert-Phone (-not ($script:Messages -join '|').Contains($script:InputValue)) 'Full value was printed.'

Reset-PhoneTest
$script:InputValue = 'synthetic-replacement-1234'
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $true $true) -eq 'written') 'Explicit replacement failed.'

Reset-PhoneTest
$script:InputValue = 'AC' + ('1' * 32)
$script:VerifyName='TWILIO_ACCOUNT_SID'
Assert-Phone ((Set-PhoneValue 'TWILIO_ACCOUNT_SID' 'variable' $false) -eq 'written') 'SID variable failed.'
Assert-Phone ($script:Calls[0].Args[0] -eq 'variable') 'Public SID went to wrong scope.'

Reset-PhoneTest
$script:InputValue='not-a-sid'
Assert-Phone ((Set-PhoneValue 'TWILIO_API_KEY_SID' 'variable' $false) -eq 'failed') 'Invalid SID accepted.'
Assert-Phone ($script:Calls.Count -eq 0) 'Invalid SID wrote a value.'

Reset-PhoneTest
$script:InputValue="bad`nvalue"
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $false) -eq 'failed') 'Multiline accepted.'
Assert-Phone ($script:Calls.Count -eq 0) 'Multiline wrote a value.'

Reset-PhoneTest
$script:InputValue='synthetic-failure-1234'; $script:FailWrite=$true
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $false) -eq 'failed') 'Write failure claimed success.'
Assert-Phone ($script:Calls.Count -eq 1) 'Failed write retried.'

Reset-PhoneTest
$script:FailVerify=$true
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $false) -eq 'failed') 'Verification failure claimed success.'

Reset-PhoneTest
$script:VerifyName='SOME_OTHER_NAME'
Assert-Phone ((Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $false) -eq 'failed') 'Missing saved name claimed success.'

Reset-PhoneTest
$script:InputValue='abc'
$null = Set-PhoneValue 'TWILIO_AUTH_TOKEN' 'secret' $false
Assert-Phone (-not ($script:Messages -join '|').Contains('last four: abc')) 'Short value disclosed in full.'
[Console]::WriteLine('PASS: stdin roundtrip and 11 offline credential checks; no provider calls.')
