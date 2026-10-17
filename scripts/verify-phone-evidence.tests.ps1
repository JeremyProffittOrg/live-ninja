# Offline fixtures only; no provider calls or subprocesses.
$ErrorActionPreference='Stop'
. "$PSScriptRoot/verify-phone-evidence.ps1"
$testRoot=Join-Path ([IO.Path]::GetTempPath()) ('live-ninja-phone-evidence-'+[Guid]::NewGuid().ToString('N'))
$null=New-Item -ItemType Directory -Path $testRoot
$now=[DateTimeOffset]::UtcNow; $sha='a'*40; $owner='12345678-1234-1234-1234-123456789012'; $script:checks=0
function Get-PhoneEvidenceHead { return $sha }
function Save-TestJson { param([string]$Path,$Value)
 [IO.File]::WriteAllText($Path,($Value | ConvertTo-Json -Depth 30),[Text.UTF8Encoding]::new($false))
}
function Assert-Fails { param([scriptblock]$Action,[string]$Pattern)
 $failure=$null
 try { & $Action } catch { $failure=$_.Exception.Message }
 if (-not $failure -or $failure -notmatch $Pattern) { throw "Expected '$Pattern', received '$failure'." }
 $script:checks++
}
$manifest=[ordered]@{schemaVersion=1;repository='JeremyProffittOrg/live-ninja';awsAccount='759775734231';azureSubscription='adc40fff-bab3-4bd2-b961-1832d0375052';ownerId=$owner;commit=$sha;legacyDevice='4633424442303098';synthetic=$true;createdAt=$now.ToString('o');expiresAt=$now.AddHours(1).ToString('o');callsHeld=$true;executionEnabled=$false;budget=@{monthlyUSD=20;dailyGrantUSD=5};approvedTargets=@();attempts=[ordered]@{}}
$manifest.implementationFiles=@('verify-phone-evidence.bat','verify-phone-evidence.ps1','verify-phone-evidence.tests.ps1','verify-phone-evidence.schema.json') | ForEach-Object { @{path=('scripts/'+$_);sha256=(Get-FileHash -LiteralPath (Join-Path $PSScriptRoot $_)).Hash.ToLowerInvariant()} }
$receipts=@{}
try {
 $milestoneNames=@($script:PhoneEvidenceSchema.milestones.PSObject.Properties.Name)
 foreach ($entry in $script:PhoneEvidenceSchema.milestones.PSObject.Properties) {
  foreach ($dependency in $entry.Value.depends) {
   Assert-PhoneEvidence ($milestoneNames -ccontains $dependency) ('Unknown schema dependency: '+$dependency)
  }
  $activeNames=@($entry.Name)+@($entry.Value.items)+@($entry.Value.cases)+@($entry.Value.depends)
  Assert-PhoneEvidence (@($activeNames | Where-Object { $_ -cmatch '^auto-' }).Count -eq 0) 'Android Auto belongs in the backlog, not active evidence gates.'
 }
 $script:checks++
 foreach ($entry in $script:PhoneEvidenceSchema.milestones.PSObject.Properties) {
  $name=$entry.Name; $spec=$entry.Value
  $relative='attempts/'+$now.ToString('yyyyMMddTHHmmssZ')+'-'+[Guid]::NewGuid().ToString('N')
  $directory=Join-Path $testRoot $relative; $null=New-Item -ItemType Directory -Path $directory
  $stream=[IO.File]::Open((Join-Path $directory 'output.txt'),[IO.FileMode]::CreateNew)
  $bytes=[Text.Encoding]::UTF8.GetBytes((@('PASS: 25 phone evidence checks; synthetic fixtures only; no provider calls.','ok  synthetic/package 0.1s','# pass 1','# fail 0','BUILD SUCCESSFUL','Synthetic observation only.') -join [Environment]::NewLine))
  $stream.Write($bytes,0,$bytes.Length); $stream.Dispose()
  $source=@{path='output.txt';sha256=(Get-FileHash -LiteralPath (Join-Path $directory 'output.txt')).Hash.ToLowerInvariant()}
  $receipt=@{schemaVersion=1;milestone=$name;commit=$sha;ownerId=$owner;awsAccount=$manifest.awsAccount;azureSubscription=$manifest.azureSubscription;synthetic=$true;status='completed';completedAt=$now.ToString('o');commands=@($spec.checks | ForEach-Object { @{command=$_;exitCode=0;status='completed';output=$source} });items=@($spec.items | ForEach-Object { @{name=$_;source=$source} });cases=@($spec.cases | ForEach-Object { @{name=$_;device=$manifest.legacyDevice;source=$source;human=@{observer='synthetic';uplink='synthetic phrase';downlink='synthetic phrase';source=$source}} })}
  $manifest.attempts[$name]=$relative; $receipts[$name]=$receipt
  Save-TestJson (Join-Path $directory 'receipt.json') $receipt
 }
 $mp=Join-Path $testRoot 'manifest.json'; Save-TestJson $mp $manifest
 Test-PhoneEvidenceGate 'phone-verification-tools' $testRoot $sha -SyntheticTest -Now $now; $script:checks++
 Test-PhoneEvidenceGate 'car-repro' $testRoot $sha -SyntheticTest -Now $now; $script:checks++
 Assert-Fails { Test-PhoneEvidenceGate 'phone-verification-tools' $testRoot $sha -Now $now } 'Synthetic evidence'
 Assert-Fails { Test-PhoneEvidenceGate 'unknown' $testRoot $sha -SyntheticTest -Now $now } 'Unknown milestone'
 Assert-Fails { Test-PhoneEvidenceGate 'phone-verification-tools' (Join-Path $testRoot 'absent') $sha -SyntheticTest -Now $now } 'does not exist|Cannot find'
 Assert-Fails { Test-PhoneEvidenceGate 'phone-verification-tools' $testRoot ('b'*40) -SyntheticTest -Now $now } 'Wrong checkout commit'
 foreach ($t in @(@('awsAccount','000000000000','Wrong provider'),@('ownerId','unresolved','Unresolved owner'),@('legacyDevice','wrong','Wrong legacy'),@('repository','other/repo','Wrong repository'),@('createdAt',$now.AddDays(-2).ToString('o'),'Stale'),@('expiresAt',$now.AddSeconds(-1).ToString('o'),'Expired'))) {
  $old=$manifest[$t[0]]; $manifest[$t[0]]=$t[1]; Save-TestJson $mp $manifest
  Assert-Fails { Test-PhoneEvidenceGate 'phone-verification-tools' $testRoot $sha -SyntheticTest -Now $now } $t[2]
  $manifest[$t[0]]=$old; Save-TestJson $mp $manifest
 }
 $name='phone-verification-tools'; $r=$receipts[$name]; $rp=Join-Path $testRoot ($manifest.attempts[$name]+'/receipt.json')
 foreach ($t in @(@('ownerId','87654321-1234-1234-1234-123456789012','Receipt commit or account'),@('status','skipped','Incomplete or skipped'),@('commit',('c'*40),'Receipt commit or account'))) {
  $old=$r[$t[0]]; $r[$t[0]]=$t[1]; Save-TestJson $rp $r
  Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } $t[2]
  $r[$t[0]]=$old; Save-TestJson $rp $r
 }
 $r.commands[0].exitCode=1; Save-TestJson $rp $r
 Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } 'Failed or skipped command'
 $r.commands[0].exitCode=0; $old=$r.commands[0].output.sha256; $r.commands[0].output.sha256='0'*64; Save-TestJson $rp $r
 Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } 'hash mismatch'
 $r.commands[0].output.sha256=$old; $oldPath=$r.commands[0].output.path; $r.commands[0].output.path='../output.txt'; Save-TestJson $rp $r
 Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } 'escapes attempt'
 $r.commands[0].output.path=$oldPath; $savedItems=$r.items; $r.items=@(); Save-TestJson $rp $r
 Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } 'Missing or duplicate acceptance'
 $r.items=$savedItems; Save-TestJson $rp $r
 $savedCommands=$r.commands; $r.commands=@(); Save-TestJson $rp $r
 Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } 'Missing or duplicate required command'
 $r.commands=$savedCommands; $r.commands[0].status='skipped'; Save-TestJson $rp $r
 Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } 'Failed or skipped command'
 $r.commands[0].status='completed'; Save-TestJson $rp $r
 $oldHash=$manifest.implementationFiles[0].sha256; $manifest.implementationFiles[0].sha256='0'*64; Save-TestJson $mp $manifest
 Assert-Fails { Test-PhoneEvidenceGate $name $testRoot $sha -SyntheticTest -Now $now } 'hash mismatch'
 $manifest.implementationFiles[0].sha256=$oldHash; Save-TestJson $mp $manifest
 $cr=$receipts['car-repro']; $crp=Join-Path $testRoot ($manifest.attempts['car-repro']+'/receipt.json')
 $human=$cr.cases[0].human; $cr.cases[0].human=$null; Save-TestJson $crp $cr
 Assert-Fails { Test-PhoneEvidenceGate 'car-repro' $testRoot $sha -SyntheticTest -Now $now } 'Missing human'
 $cr.cases[0].human=$human; $cr.cases[0].device='wrong'; Save-TestJson $crp $cr
 Assert-Fails { Test-PhoneEvidenceGate 'car-repro' $testRoot $sha -SyntheticTest -Now $now } 'Wrong scenario device'
 $cr.cases[0].device=$manifest.legacyDevice; Save-TestJson $crp $cr
 Assert-Fails { Invoke-PhoneEvidenceCase 'not-a-case' $testRoot } 'Unknown scenario'
 Assert-Fails { Invoke-PhoneEvidenceCase 'phone-live-call' $testRoot } 'held by the owner'
 $manifest.callsHeld=$false; Save-TestJson $mp $manifest
 Assert-Fails { Invoke-PhoneEvidenceCase 'phone-live-call' $testRoot } 'Missing locked target'
 $manifest.executionEnabled=$true; $manifest.approvedTargets=@('synthetic-only'); Save-TestJson $mp $manifest
 Assert-Fails { Invoke-PhoneEvidenceCase 'phone-live-call' $testRoot } 'Wrong checkout commit|Synthetic evidence'
 $manifest.synthetic=$false; Save-TestJson $mp $manifest
 foreach ($n in @('phone-verification-tools','car-usb-access')) { $receipts[$n].synthetic=$false; Save-TestJson (Join-Path $testRoot ($manifest.attempts[$n]+'/receipt.json')) $receipts[$n] }
 Assert-Fails { Test-PhoneEvidenceGate 'car-usb-access' $testRoot $sha -Now $now } 'Missing independent source references'


 # Current Node's spec reporter is also accepted, without treating skipped tests as proof.
 $nodeReceipt=$receipts['call-client']; $nodePath=Join-Path $testRoot ($manifest.attempts['call-client']+'/output.txt')
 $nodeText=[IO.File]::ReadAllText($nodePath).Replace('# pass 1',([char]0x2139)+' pass 1').Replace('# fail 0',([char]0x2139)+' fail 0')
 [IO.File]::WriteAllText($nodePath,$nodeText,[Text.UTF8Encoding]::new($false))
 $nodeReceipt.commands[0].output.sha256=(Get-FileHash -LiteralPath $nodePath).Hash.ToLowerInvariant()
 Save-TestJson (Join-Path $testRoot ($manifest.attempts['call-client']+'/receipt.json')) $nodeReceipt
 $manifest.synthetic=$true
 foreach ($n in @('phone-verification-tools','car-usb-access')) { $receipts[$n].synthetic=$true; Save-TestJson (Join-Path $testRoot ($manifest.attempts[$n]+'/receipt.json')) $receipts[$n] }
 Save-TestJson $mp $manifest
 Test-PhoneEvidenceGate 'call-client' $testRoot $sha -SyntheticTest -Now $now; $script:checks++

 # Mock only the read boundary: adapter binding decisions run unchanged and never launch tools.
 $script:readMode='ok'; $script:readCalls=0
 function Invoke-PhoneEvidenceRead { param([string]$Tool,[string[]]$Arguments)
  $script:readCalls++
  if ($Tool -eq 'gh') {
   if ($Arguments[0] -eq 'run') {
    $loggedStep=if ($script:readMode -eq 'wrong-log-step') {'unrelated-step'} else {'verify'}
    return "build`t$loggedStep`t2026-10-17T12:00:00Z verify-source-command"
   }
   if ($Arguments[1].Contains('/jobs?')) {
    return '{"total_count":1,"jobs":[{"id":7,"conclusion":"success","steps":[{"name":"verify","status":"completed","conclusion":"success"}]}]}'
   }
   $commit=if ($script:readMode -eq 'wrong-sha') {'b'*40} else {$sha}
   return (@{repository=@{full_name='JeremyProffittOrg/live-ninja'};head_sha=$commit;status='completed';conclusion='success'} | ConvertTo-Json -Compress)
  }
  if ($Tool -eq 'aws') {
   if ($Arguments[0] -eq 'sts') { return (@{Account=$(if($script:readMode -eq 'wrong-account'){'000000000000'}else{'759775734231'})} | ConvertTo-Json -Compress) }
   return (@{Item=@{ownerUserId=@{S=$owner}}} | ConvertTo-Json -Compress)
  }
  throw 'Unexpected fixture tool read.'
 }
 $sourcePath=Join-Path $testRoot 'live-source.json'
 $liveSource=@{milestone='phone-azure-service';githubRunId='123';checks=@(@{command='verify-source-command';jobId='7';stepName='verify'})}
 Save-TestJson $sourcePath $liveSource
 $liveReceipt=@{live=@{source=@{path='live-source.json';sha256=(Get-FileHash -LiteralPath $sourcePath).Hash.ToLowerInvariant()}};cases=@()}
 $requirements=@{checks=@('verify-source-command')}
 Test-PhoneEvidenceLive $manifest $liveReceipt $requirements $testRoot 'phone-azure-service'; $script:checks++
 Assert-Fails { Test-PhoneEvidenceLive $manifest $liveReceipt $requirements $testRoot 'car-usb-access' } 'another milestone'
 $script:readMode='wrong-sha'
 Assert-Fails { Test-PhoneEvidenceLive $manifest $liveReceipt $requirements $testRoot 'phone-azure-service' } 'commit, or result mismatch'
 $script:readMode='wrong-account'
 Assert-Fails { Test-PhoneEvidenceLive $manifest $liveReceipt $requirements $testRoot 'phone-azure-service' } 'another account'
 $script:readMode='ok'
 Assert-Fails { Test-PhoneEvidenceLive $manifest $liveReceipt @{checks=@('other-command')} $testRoot 'phone-azure-service' } 'Missing independent command binding'
 $script:readMode='wrong-log-step'
 Assert-Fails { Test-PhoneEvidenceLive $manifest $liveReceipt $requirements $testRoot 'phone-azure-service' } 'Provider step log lacks required command'
 $script:readMode='ok'
 $liveSource.checks[0].stepName='unrelated-step'; Save-TestJson $sourcePath $liveSource
 $liveReceipt.live.source.sha256=(Get-FileHash -LiteralPath $sourcePath).Hash.ToLowerInvariant()
 Assert-Fails { Test-PhoneEvidenceLive $manifest $liveReceipt $requirements $testRoot 'phone-azure-service' } 'step did not succeed'

 [Console]::WriteLine("PASS: $script:checks phone evidence checks; synthetic fixtures only; no provider calls.")
} finally {
 $safeRoot=[IO.Path]::GetFullPath($testRoot); $tempBase=[IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\')+'\'
 if (-not $safeRoot.StartsWith($tempBase,[StringComparison]::OrdinalIgnoreCase) -or [IO.Path]::GetFileName($safeRoot) -notmatch '^live-ninja-phone-evidence-[a-f0-9]{32}$') { throw 'Unsafe fixture cleanup path.' }
 Remove-Item -LiteralPath $safeRoot -Recurse -Force
}
