# Read-only evidence validation. This script never starts a telephone call.
# Layout: <EvidenceRoot>/manifest.json and attempts/<UTC yyyyMMddTHHmmssZ>-<32hex>/receipt.json.
# Each manifest.attempts property maps an exact milestone to its immutable attempt directory.
# Receipt commands contain command, exitCode, status=completed, and output={path,sha256}.
# Receipt items/cases contain name and source={path,sha256}; case human observations are separate.
# A hash proves file integrity, not provider truth. Live receipts also bind exact commands
# to GitHub job logs and compare current AWS/device identities. Synthetic fixtures are
# accepted only by the dot-sourced test API. run-case validates captured evidence only.
[CmdletBinding()]
param([Parameter(Position=0)][string]$Milestone,
      [Parameter(Position=1)][string]$Case,
      [string]$EvidenceRoot='C:\dev\live-ninja-test-evidence\phone-plan-2026-10-15')
$ErrorActionPreference='Stop'
$script:PhoneEvidenceSchema=Get-Content -LiteralPath "$PSScriptRoot/verify-phone-evidence.schema.json" -Raw | ConvertFrom-Json

function Assert-PhoneEvidence { param([bool]$Condition,[string]$Reason)
    if (-not $Condition) { throw $Reason }
}
function Get-PhoneEvidenceFile { param([string]$Base,[string]$Relative)
    Assert-PhoneEvidence (-not [string]::IsNullOrWhiteSpace($Relative)) 'Missing evidence path.'
    Assert-PhoneEvidence (-not [IO.Path]::IsPathRooted($Relative)) 'Evidence path must be relative.'
    $basePath=[IO.Path]::GetFullPath($Base).TrimEnd('\','/')
    $path=[IO.Path]::GetFullPath((Join-Path $basePath $Relative))
    Assert-PhoneEvidence ($path.StartsWith($basePath+[IO.Path]::DirectorySeparatorChar,[StringComparison]::OrdinalIgnoreCase)) 'Evidence path escapes attempt.'
    $cursor=$path
    while ($cursor.Length -ge $basePath.Length) {
        $entry=Get-Item -LiteralPath $cursor -Force
        Assert-PhoneEvidence (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) 'Reparse-point evidence is not accepted.'
        $cursor=[IO.Path]::GetDirectoryName($cursor)
    }
    Assert-PhoneEvidence ([IO.File]::Exists($path)) 'Evidence source is not a file.'
    return $path
}
function Read-PhoneEvidenceJson { param([string]$Path)
    Assert-PhoneEvidence ((Get-Item -LiteralPath $Path).Length -le 2097152) 'Evidence JSON exceeds size limit.'
    return (Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json)
}
function Test-PhoneEvidenceSource { param([string]$Base,$Reference)
    Assert-PhoneEvidence ($null -ne $Reference -and $Reference.sha256 -cmatch '^[a-f0-9]{64}$') 'Missing SHA256 provenance.'
    $file=Get-PhoneEvidenceFile $Base $Reference.path
    Assert-PhoneEvidence ((Get-Item -LiteralPath $file).Length -gt 0) 'Empty evidence source.'
    Assert-PhoneEvidence ((Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant() -ceq $Reference.sha256) 'Evidence source hash mismatch.'
    return $file
}
function Get-PhoneEvidenceHead {
    $p=New-Object Diagnostics.Process
    $p.StartInfo=New-Object Diagnostics.ProcessStartInfo
    $p.StartInfo.FileName='git.exe'
    $p.StartInfo.Arguments='rev-parse HEAD'
    $p.StartInfo.WorkingDirectory=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
    $p.StartInfo.UseShellExecute=$false; $p.StartInfo.CreateNoWindow=$true
    $p.StartInfo.RedirectStandardOutput=$true; $p.StartInfo.RedirectStandardError=$true
    $null=$p.Start(); $output=$p.StandardOutput.ReadToEnd(); $null=$p.StandardError.ReadToEnd()
    if (-not $p.WaitForExit(10000)) { $p.Kill(); throw 'Git identity check timed out.' }
    Assert-PhoneEvidence ($p.ExitCode -eq 0) 'Cannot verify checkout commit.'
    $p.Dispose(); return $output.Trim()
}
function Test-PhoneEvidenceTime { param([string]$Value,[DateTimeOffset]$Now)
    $time=[DateTimeOffset]::Parse($Value)
    Assert-PhoneEvidence ($time.Offset -eq [TimeSpan]::Zero -and $time -le $Now.AddMinutes(5) -and $time -ge $Now.AddHours(-24)) 'Stale or future evidence.'
}


# Only these read-only executables/argument families are constructed by this file.
function Invoke-PhoneEvidenceRead {
    param([ValidateSet('git','gh','aws','adb')][string]$Tool,[string[]]$Arguments)
    $exe=switch ($Tool) {
        'adb' { 'C:\Users\Jeremy\AppData\Local\Android\Sdk\platform-tools\adb.exe' }
        'aws' { 'C:\Program Files\Amazon\AWSCLIV2\aws.exe' }
        default { (Get-Command ($Tool+'.exe') -CommandType Application -ErrorAction Stop).Source }
    }
    Assert-PhoneEvidence ([IO.File]::Exists($exe)) ('Missing read-only tool: '+$Tool)
    $p=New-Object Diagnostics.Process
    $p.StartInfo=New-Object Diagnostics.ProcessStartInfo
    $p.StartInfo.FileName=$exe
    # Windows CommandLineToArgvW quoting, without a shell or command expansion.
    $quoted=@($Arguments | ForEach-Object { '"'+([regex]::Replace([regex]::Replace($_,'(\\*)"','$1$1\"'),'(\\+)$','$1$1'))+'"' })
    $p.StartInfo.Arguments=$quoted -join ' '
    $p.StartInfo.UseShellExecute=$false; $p.StartInfo.CreateNoWindow=$true
    $p.StartInfo.RedirectStandardOutput=$true; $p.StartInfo.RedirectStandardError=$true
    $p.StartInfo.EnvironmentVariables['AWS_PAGER']=''
    $p.StartInfo.EnvironmentVariables['GH_PROMPT_DISABLED']='1'
    $null=$p.Start()
    $stdout=$p.StandardOutput.ReadToEndAsync(); $stderr=$p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit(30000)) { $p.Kill(); throw ($Tool+' read-only verification timed out.') }
    $output=$stdout.Result; $null=$stderr.Result
    Assert-PhoneEvidence ($p.ExitCode -eq 0) ($Tool+' read-only verification failed; check configured sign-in/access or device connection.')
    $p.Dispose(); return $output
}
function Test-PhoneEvidenceLive {
    param($Manifest,$Receipt,$Requirements,[string]$Attempt,[string]$Name)
    Assert-PhoneEvidence ($null -ne $Receipt.live -and $null -ne $Receipt.live.source) ('Missing independent source references for '+$Name)
    $live=Read-PhoneEvidenceJson (Test-PhoneEvidenceSource $Attempt $Receipt.live.source)
    Assert-PhoneEvidence ($live.milestone -ceq $Name) 'Independent evidence belongs to another milestone.'
    Assert-PhoneEvidence ($live.githubRunId -match '^[1-9][0-9]*$') 'Missing GitHub run identifier.'
    $repo=$script:PhoneEvidenceSchema.repository
    $run=Invoke-PhoneEvidenceRead gh @('api',('repos/'+$repo+'/actions/runs/'+$live.githubRunId)) | ConvertFrom-Json
    Assert-PhoneEvidence ($run.repository.full_name -ceq $repo -and $run.head_sha -ceq $Manifest.commit -and $run.status -ceq 'completed' -and $run.conclusion -ceq 'success') 'GitHub run repository, commit, or result mismatch.'
    $jobs=Invoke-PhoneEvidenceRead gh @('api',('repos/'+$repo+'/actions/runs/'+$live.githubRunId+'/jobs?per_page=100')) | ConvertFrom-Json
    Assert-PhoneEvidence ($jobs.total_count -le 100) 'GitHub jobs require explicit pagination reconciliation.'
    # Each requested check must identify its successful job/step, and its exact command
    # must appear in that job's provider log. A generic green workflow cannot pass.
    foreach ($command in $Requirements.checks) {
        $bindings=@($live.checks | Where-Object { $_.command -ceq $command })
        Assert-PhoneEvidence ($bindings.Count -eq 1) ('Missing independent command binding: '+$command)
        $binding=$bindings[0]
        Assert-PhoneEvidence ($binding.jobId -match '^[1-9][0-9]*$' -and $binding.stepName) 'Missing GitHub job/step binding.'
        $job=@($jobs.jobs | Where-Object { [string]$_.id -ceq [string]$binding.jobId })
        Assert-PhoneEvidence ($job.Count -eq 1 -and $job[0].conclusion -ceq 'success') 'Bound GitHub job did not succeed.'
        $step=@($job[0].steps | Where-Object { $_.name -ceq $binding.stepName -and $_.status -ceq 'completed' -and $_.conclusion -ceq 'success' })
        Assert-PhoneEvidence ($step.Count -eq 1) 'Bound GitHub verification step did not succeed.'
        $log=Invoke-PhoneEvidenceRead gh @('run','view','--repo',$repo,'--job',[string]$binding.jobId,'--log')
        # gh prefixes each log line with job and step names separated by tabs.
        # A command in another successful step does not prove this binding.
        $stepCommands=@($log -split "`n" | Where-Object {
            $fields=$_ -split "`t",3
            $fields.Count -eq 3 -and $fields[1] -ceq $binding.stepName -and $fields[2].Contains($command)
        })
        Assert-PhoneEvidence ($stepCommands.Count -gt 0) ('Provider step log lacks required command: '+$command)
    }
    $identity=Invoke-PhoneEvidenceRead aws @('sts','get-caller-identity','--output','json') | ConvertFrom-Json
    Assert-PhoneEvidence ($identity.Account -ceq $Manifest.awsAccount) 'Configured AWS identity belongs to another account.'
    $owner=Invoke-PhoneEvidenceRead aws @('dynamodb','get-item','--region','us-east-1','--table-name','live-ninja-phone-trial','--key','{"PK":{"S":"CONFIG"},"SK":{"S":"OWNER_MESSAGES"}}','--consistent-read','--projection-expression','ownerUserId','--output','json') | ConvertFrom-Json
    Assert-PhoneEvidence ($owner.Item.ownerUserId.S -ceq $Manifest.ownerId) 'Provider owner configuration does not match receipt.'
    foreach ($case in $Receipt.cases) {
        $sources=@($live.cases | Where-Object { $_.name -ceq $case.name })
        Assert-PhoneEvidence ($sources.Count -eq 1) ('Missing independent scenario binding: '+$case.name)
        $source=$sources[0]
        Assert-PhoneEvidence ($source.runId -match '^[A-Za-z0-9_:#-]{1,150}$' -and $source.callSid -cmatch '^CA[a-f0-9]{32}$') 'Missing call journal identity.'
        $key=@{PK=@{S=('RUN#'+$source.runId)};SK=@{S=('CALL#'+$source.callSid)}} | ConvertTo-Json -Compress
        $journal=Invoke-PhoneEvidenceRead aws @('dynamodb','get-item','--region','us-east-1','--table-name','live-ninja-phone-trial','--key',$key,'--consistent-read','--projection-expression','runId,callSid,targetId,recordingStatus','--output','json') | ConvertFrom-Json
        Assert-PhoneEvidence ($journal.Item.runId.S -ceq $source.runId -and $journal.Item.callSid.S -ceq $source.callSid -and $journal.Item.targetId.S -ceq $source.targetId -and $source.targetId) 'Call journal does not match captured scenario target.'
        Assert-PhoneEvidence ($source.recordingStatus -in @('ready','absent','failed','declined') -and $journal.Item.recordingStatus.S -ceq $source.recordingStatus) 'Call recording journal outcome mismatch.'
    }
    if ($Name.StartsWith('car-') -or $Name.StartsWith('auto-')) {
        $device=$live.device
        Assert-PhoneEvidence ($device.serial -ceq $Manifest.legacyDevice -or ($Manifest.modernDevice -and $device.serial -ceq $Manifest.modernDevice)) 'Missing bound device reference.'
        $state=Invoke-PhoneEvidenceRead adb @('-s',[string]$device.serial,'get-state')
        Assert-PhoneEvidence ($state.Trim() -ceq 'device') 'Bound Android device is not online.'
        $sdk=Invoke-PhoneEvidenceRead adb @('-s',[string]$device.serial,'shell','getprop','ro.build.version.sdk')
        if ($Name.StartsWith('auto-')) { Assert-PhoneEvidence ([int]$sdk.Trim() -ge 30 -and $device.serial -ceq $Manifest.modernDevice) 'Android Auto requires the locked Android 11+ phone.' }
        $package=Invoke-PhoneEvidenceRead adb @('-s',[string]$device.serial,'shell','dumpsys','package','ninja.jeremy.liveninja')
        Assert-PhoneEvidence ($device.versionCode -match '^[0-9]+$' -and $package -match ('versionCode='+[regex]::Escape([string]$device.versionCode)+'\b')) 'Installed Android version code mismatch.'
        Assert-PhoneEvidence ($device.versionName -and $package -match ('versionName='+[regex]::Escape([string]$device.versionName)+'(?:\s|$)')) 'Installed Android version name mismatch.'
        Assert-PhoneEvidence ($device.apkSha256 -cmatch '^[a-f0-9]{64}$') 'Missing installed artifact SHA256.'
        $apk=Invoke-PhoneEvidenceRead adb @('-s',[string]$device.serial,'shell','pm','path','ninja.jeremy.liveninja')
        Assert-PhoneEvidence ($apk.Trim() -cmatch '^package:(/data/app/[A-Za-z0-9_=/.-]+/base\.apk)$') 'Installed package path is missing or ambiguous.'
        $apkPath=$Matches[1]
        $hash=Invoke-PhoneEvidenceRead adb @('-s',[string]$device.serial,'shell','sha256sum',$apkPath)
        Assert-PhoneEvidence ($hash -cmatch ('^'+$device.apkSha256+'\s')) 'Installed artifact hash mismatch.'
    }
}

function Test-PhoneEvidenceGate {
    param([string]$Name,[string]$Root,[string]$ExpectedCommit,[switch]$SyntheticTest,
          [DateTimeOffset]$Now=[DateTimeOffset]::UtcNow,[string[]]$Visited=@())
    $spec=$script:PhoneEvidenceSchema.milestones.PSObject.Properties[$Name]
    Assert-PhoneEvidence ($null -ne $spec) 'Unknown milestone.'
    Assert-PhoneEvidence ($Visited -notcontains $Name) 'Cyclic evidence dependency.'
    $requirements=$spec.Value
    $manifest=Read-PhoneEvidenceJson (Get-PhoneEvidenceFile $Root 'manifest.json')
    Assert-PhoneEvidence ($manifest.schemaVersion -eq 1) 'Unsupported manifest schema.'
    Assert-PhoneEvidence ($manifest.repository -ceq $script:PhoneEvidenceSchema.repository) 'Wrong repository.'
    Assert-PhoneEvidence ($manifest.awsAccount -ceq $script:PhoneEvidenceSchema.awsAccount -and $manifest.azureSubscription -ceq $script:PhoneEvidenceSchema.azureSubscription) 'Wrong provider account.'
    Assert-PhoneEvidence ($manifest.ownerId -match '^[a-f0-9-]{36}$') 'Unresolved owner identity.'
    Assert-PhoneEvidence ($ExpectedCommit -cmatch '^[a-f0-9]{40}$' -and $manifest.commit -ceq $ExpectedCommit) 'Wrong checkout commit.'
    Assert-PhoneEvidence ($manifest.legacyDevice -ceq $script:PhoneEvidenceSchema.legacyDevice) 'Wrong legacy device.'
    Assert-PhoneEvidence ($manifest.synthetic -is [bool]) 'Missing evidence classification.'
    Assert-PhoneEvidence (-not $manifest.synthetic -or $SyntheticTest) 'Synthetic evidence cannot satisfy a live gate.'
    Assert-PhoneEvidence (-not $SyntheticTest -or $manifest.synthetic) 'Test mode requires synthetic fixtures.'
    foreach ($requiredFile in @('scripts/verify-phone-evidence.bat','scripts/verify-phone-evidence.ps1','scripts/verify-phone-evidence.tests.ps1','scripts/verify-phone-evidence.schema.json')) {
        $files=@($manifest.implementationFiles | Where-Object { $_.path -ceq $requiredFile })
        Assert-PhoneEvidence ($files.Count -eq 1) 'Missing implementation source provenance.'
        $null=Test-PhoneEvidenceSource ([IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))) $files[0]
    }
    Test-PhoneEvidenceTime $manifest.createdAt $Now
    Assert-PhoneEvidence ([DateTimeOffset]::Parse($manifest.expiresAt) -gt $Now) 'Expired run manifest.'
    Assert-PhoneEvidence ($manifest.callsHeld -is [bool] -and $manifest.executionEnabled -is [bool]) 'Unresolved execution authority.'
    Assert-PhoneEvidence ($null -ne $manifest.budget -and $manifest.budget.monthlyUSD -eq 20 -and $manifest.budget.dailyGrantUSD -eq 5) 'Missing or incorrect budget limits.'
    $binding=$manifest.attempts.PSObject.Properties[$Name]
    Assert-PhoneEvidence ($null -ne $binding) 'Missing milestone attempt.'
    $relative=[string]$binding.Value
    Assert-PhoneEvidence ($relative -cmatch '^attempts/[0-9]{8}T[0-9]{6}Z-[a-f0-9]{32}$') 'Attempt must have an immutable UTC/UUID path.'
    $receiptPath=Get-PhoneEvidenceFile $Root ($relative+'/receipt.json')
    $attempt=[IO.Path]::GetDirectoryName($receiptPath)
    $receipt=Read-PhoneEvidenceJson $receiptPath
    Assert-PhoneEvidence ($receipt.schemaVersion -eq 1 -and $receipt.milestone -ceq $Name) 'Wrong receipt identity.'
    Assert-PhoneEvidence ($receipt.commit -ceq $ExpectedCommit -and $receipt.ownerId -ceq $manifest.ownerId -and $receipt.awsAccount -ceq $manifest.awsAccount -and $receipt.azureSubscription -ceq $manifest.azureSubscription) 'Receipt commit or account mismatch.'
    Assert-PhoneEvidence ($receipt.synthetic -is [bool] -and $receipt.synthetic -eq $manifest.synthetic) 'Receipt classification mismatch.'
    Assert-PhoneEvidence ($receipt.status -ceq 'completed') 'Incomplete or skipped attempt.'
    Test-PhoneEvidenceTime $receipt.completedAt $Now
    foreach ($check in $requirements.checks) {
        $matches=@($receipt.commands | Where-Object { $_.command -ceq $check })
        Assert-PhoneEvidence ($matches.Count -eq 1) 'Missing or duplicate required command.'
        $command=$matches[0]
        Assert-PhoneEvidence ($command.exitCode -is [int] -and $command.exitCode -eq 0 -and $command.status -ceq 'completed') 'Failed or skipped command.'
        $output=Test-PhoneEvidenceSource $attempt $command.output
        $text=Get-Content -LiteralPath $output -Raw -Encoding UTF8
        Assert-PhoneEvidence ($text -notmatch '(?m)^FAIL\b|^not ok\b|BUILD FAILED|\[SKIP\]') 'Command output contains failure or skipped evidence.'
        # Output semantics are checked in addition to exit status and hashes.
        if ($check.StartsWith('go test ')) { Assert-PhoneEvidence ($text -match '(?m)^ok\s+') 'Missing Go test result.' }
        elseif ($check.StartsWith('node --test ')) {
            Assert-PhoneEvidence ($text -match '(?m)^(?:#|\u2139) fail 0\s*$' -and $text -match '(?m)^(?:#|\u2139) pass [1-9][0-9]*\s*$') 'Missing Node TAP/spec test results.'
            Assert-PhoneEvidence ($text -notmatch '(?m)^(?:#|\u2139) skipped [1-9][0-9]*\s*$') 'Skipped Node tests cannot pass a gate.'
        }
        elseif ($check.Contains('gradlew.bat')) { Assert-PhoneEvidence ($text.Contains('BUILD SUCCESSFUL')) 'Missing Gradle result.' }
        elseif ($Name -eq 'phone-verification-tools') { Assert-PhoneEvidence ($text -match '^PASS: [1-9][0-9]* phone evidence checks; synthetic fixtures only; no provider calls\.') 'Missing evidence test result.' }
    }
    foreach ($item in $requirements.items) {
        $matches=@($receipt.items | Where-Object { $_.name -ceq $item })
        Assert-PhoneEvidence ($matches.Count -eq 1) 'Missing or duplicate acceptance item.'
        $null=Test-PhoneEvidenceSource $attempt $matches[0].source
    }
    foreach ($caseName in $requirements.cases) {
        $matches=@($receipt.cases | Where-Object { $_.name -ceq $caseName })
        Assert-PhoneEvidence ($matches.Count -eq 1) 'Missing or duplicate required scenario.'
        $caseEvidence=$matches[0]
        Assert-PhoneEvidence ($caseEvidence.device -ceq $manifest.legacyDevice -or ($manifest.modernDevice -and $caseEvidence.device -ceq $manifest.modernDevice)) 'Wrong scenario device.'
        $null=Test-PhoneEvidenceSource $attempt $caseEvidence.source
        Assert-PhoneEvidence ($caseEvidence.human -and $caseEvidence.human.observer -and $caseEvidence.human.uplink -and $caseEvidence.human.downlink) 'Missing human two-way audio observation.'
        $null=Test-PhoneEvidenceSource $attempt $caseEvidence.human.source
        if (-not $SyntheticTest) {
            Assert-PhoneEvidence ($caseEvidence.human.observationType -ceq 'direct') 'Human audio evidence must explicitly record a direct observation, never inference.'
        }
    }
    foreach ($dependency in $requirements.depends) {
        Test-PhoneEvidenceGate $dependency $Root $ExpectedCommit -SyntheticTest:$SyntheticTest -Now $Now -Visited ($Visited+$Name)
    }
    if ($requirements.requiresLive -and -not $SyntheticTest) {
        Test-PhoneEvidenceLive $manifest $receipt $requirements $attempt $Name
    }
}
function Invoke-PhoneEvidenceCase { param([string]$Name,[string]$Root)
    $allowed=@($script:PhoneEvidenceSchema.milestones.PSObject.Properties | ForEach-Object { $_.Value.cases })
    Assert-PhoneEvidence ($allowed -ccontains $Name) 'Unknown scenario.'
    $manifest=Read-PhoneEvidenceJson (Get-PhoneEvidenceFile $Root 'manifest.json')
    Assert-PhoneEvidence ($manifest.callsHeld -ceq $false) 'Call execution is held by the owner.'
    Assert-PhoneEvidence ($manifest.executionEnabled -ceq $true -and @($manifest.approvedTargets).Count -gt 0 -and $manifest.budget) 'Missing locked target, budget or explicit execution flag.'
    # Compatibility name: run-case validates captured evidence; it never executes calls.
    $gate=@($script:PhoneEvidenceSchema.milestones.PSObject.Properties | Where-Object { @($_.Value.cases) -ccontains $Name })
    Assert-PhoneEvidence ($gate.Count -eq 1) 'Scenario must have one named milestone.'
    Test-PhoneEvidenceGate $gate[0].Name $Root (Get-PhoneEvidenceHead)
    Write-Output ('PASS: captured scenario '+$Name+'; no call was made.')
}
if ($MyInvocation.InvocationName -ne '.') {
    try {
        if ($Milestone -ceq 'run-case') { Invoke-PhoneEvidenceCase $Case $EvidenceRoot }
        else {
            Assert-PhoneEvidence ([string]::IsNullOrEmpty($Case)) 'Unexpected second argument.'
            Test-PhoneEvidenceGate $Milestone $EvidenceRoot (Get-PhoneEvidenceHead)
            Write-Output ('PASS: evidence gate '+$Milestone)
        }
        exit 0
    } catch { [Console]::Error.WriteLine('BLOCKED: '+$_.Exception.Message); exit 1 }
}
