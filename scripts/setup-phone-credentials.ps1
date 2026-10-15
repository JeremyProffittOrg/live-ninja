# Run through setup-phone-credentials.bat. Requires Windows PowerShell 5.1 and gh.
# No values go into command arguments, files, environment variables, or logs.
# The requested last four characters are displayed only in the user's console.
[CmdletBinding()]
param([switch]$CheckOnly, [switch]$ReplaceExisting)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$script:PhoneRepo = 'JeremyProffittOrg/live-ninja'
$script:PhoneRoot = Split-Path -Parent $PSScriptRoot

function Invoke-PhoneCli {
    param([string]$Exe, [string[]]$CliArgs, [AllowEmptyString()][string]$InputText = '')
    # All arguments are fixed command words, names, or the pinned repository.
    # Values travel over stdin, including values containing shell metacharacters.
    foreach ($arg in $CliArgs) {
        if ($arg -match '[\s"\x00]') { throw 'Invalid internal command argument.' }
    }
    $info = New-Object System.Diagnostics.ProcessStartInfo
    $info.FileName = $Exe
    $info.Arguments = $CliArgs -join ' '
    $info.WorkingDirectory = $script:PhoneRoot
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $info
    try {
        if (-not $process.Start()) { throw 'Could not start the required command.' }
        $outTask = $process.StandardOutput.ReadToEndAsync()
        $errTask = $process.StandardError.ReadToEndAsync()
        $bytes = [Text.Encoding]::UTF8.GetBytes($InputText)
        try {
            $process.StandardInput.BaseStream.Write($bytes, 0, $bytes.Length)
            $process.StandardInput.BaseStream.Flush()
            $process.StandardInput.Close()
        } finally {
            [Array]::Clear($bytes, 0, $bytes.Length)
        }
        if (-not $process.WaitForExit(60000)) {
            $process.Kill()
            throw 'Command timed out. Check GitHub before retrying a write.'
        }
        $stdout = $outTask.GetAwaiter().GetResult()
        $null = $errTask.GetAwaiter().GetResult()
        # Never relay CLI error/output text from a secret write to the console.
        return [pscustomobject]@{ Code = $process.ExitCode; Output = $stdout }
    } finally {
        $InputText = $null
        $process.Dispose()
    }
}

function Set-PhoneValue {
    param([string]$Name, [ValidateSet('secret','variable')][string]$Kind,
          [bool]$Exists, [bool]$Replace = $false)
    if ($Exists -and -not $Replace) {
        Write-Host "SKIP: $Name is already configured."
        return 'skipped'
    }
    $secure = $null
    $plain = $null
    $ptr = [IntPtr]::Zero
    try {
        $secure = Read-Host "$Name (hidden; Enter skips)" -AsSecureString
        if ($secure.Length -eq 0) {
            Write-Host "SKIP: $Name; existing value unchanged."
            return 'skipped'
        }
        $ptr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
        $plain = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr)
        $tail = if ($plain.Length -gt 4) { $plain.Substring($plain.Length - 4) } else { '[hidden: four or fewer characters]' }
        Write-Host ("Entered {0} characters; last four: {1}" -f $plain.Length, $tail)
        Write-Host 'Length confirms nonempty input, not provider validity.'
        if ($plain -match '[\r\n\x00]') {
            Write-Host "FAILED: $Name must be a single-line value. Nothing written."
            return 'failed'
        }
        if (($Name -eq 'TWILIO_ACCOUNT_SID' -and $plain -cnotmatch '^AC[0-9a-fA-F]{32}$') -or
            ($Name -eq 'TWILIO_API_KEY_SID' -and $plain -cnotmatch '^SK[0-9a-fA-F]{32}$')) {
            Write-Host "FAILED: $Name has the wrong SID format. Nothing written."
            return 'failed'
        }
        $result = Invoke-PhoneCli $script:PhoneGh @($Kind, 'set', $Name, '--repo', $script:PhoneRepo) $plain
        if ($result.Code -ne 0) {
            Write-Host "FAILED: GitHub did not confirm the write for $Name. No automatic retry."
            return 'failed'
        }
        $result = Invoke-PhoneCli $script:PhoneGh @($Kind, 'list', '--repo', $script:PhoneRepo, '--json', 'name')
        if ($result.Code -ne 0) {
            Write-Host "FAILED: $Name write succeeded, but its name could not be verified."
            return 'failed'
        }
        $names = @($result.Output | ConvertFrom-Json | ForEach-Object { $_.name })
        if ($names -notcontains $Name) {
            Write-Host "FAILED: $Name write succeeded, but its name is not listed."
            return 'failed'
        }
        Write-Host "OK: $Kind $Name saved on $script:PhoneRepo."
        return 'written'
    } finally {
        if ($ptr -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr) }
        if ($null -ne $secure) { $secure.Dispose() }
        $plain = $null
        $tail = $null
    }
}

function Invoke-PhoneSetup {
    param([bool]$Check = $false, [bool]$Replace = $false)
    try {
        Write-Host "Live Ninja phone credentials - $script:PhoneRepo"
        Write-Host 'Scope: repository Actions secrets and repository variables; no environment.'
        $script:PhoneGh = (Get-Command gh.exe -CommandType Application -ErrorAction Stop).Source
        $git = (Get-Command git.exe -CommandType Application -ErrorAction Stop).Source
        $remote = Invoke-PhoneCli $git @('remote', 'get-url', 'origin')
        if ($remote.Code -ne 0 -or $remote.Output.Trim() -notmatch '^(https://github\.com/|git@github\.com:)JeremyProffittOrg/live-ninja(?:\.git)?/?$') {
            Write-Host 'STOP: this folder does not point to JeremyProffittOrg/live-ninja.'
            return 1
        }
        $auth = Invoke-PhoneCli $script:PhoneGh @('auth', 'status', '--hostname', 'github.com')
        if ($auth.Code -ne 0) {
            Write-Host 'STOP: sign in locally with: gh auth login --hostname github.com'
            Write-Host 'Then run this batch file again. No values were requested or changed.'
            return 1
        }
        $repo = Invoke-PhoneCli $script:PhoneGh @('repo', 'view', $script:PhoneRepo, '--json', 'nameWithOwner,viewerPermission')
        if ($repo.Code -ne 0) { throw 'Repository access check failed.' }
        $repoData = $repo.Output | ConvertFrom-Json
        if ($repoData.nameWithOwner -ne $script:PhoneRepo -or $repoData.viewerPermission -ne 'ADMIN') {
            Write-Host 'STOP: repository administrator access is required to configure Actions secrets.'
            return 1
        }
        $secrets = Invoke-PhoneCli $script:PhoneGh @('secret', 'list', '--repo', $script:PhoneRepo, '--json', 'name')
        $variables = Invoke-PhoneCli $script:PhoneGh @('variable', 'list', '--repo', $script:PhoneRepo, '--json', 'name')
        if ($secrets.Code -ne 0 -or $variables.Code -ne 0) { throw 'Could not read configured names.' }
        $secretNames = @($secrets.Output | ConvertFrom-Json | ForEach-Object { $_.name })
        $variableNames = @($variables.Output | ConvertFrom-Json | ForEach-Object { $_.name })
        $items = @(
            @{ Name='TWILIO_ACCOUNT_SID'; Kind='variable'; Guide='Twilio Console > Account dashboard: Account SID (AC...). Use the same account for all four Twilio values.' },
            @{ Name='TWILIO_AUTH_TOKEN'; Kind='secret'; Guide='Twilio Console > Account dashboard: Auth Token. Used to validate Twilio webhook signatures.' },
            @{ Name='TWILIO_API_KEY_SID'; Kind='variable'; Guide='Twilio Console > Settings > Account settings > API keys & auth tokens: API key SID (SK...). Use a US1 Standard key for Voice SDK tokens.' },
            @{ Name='TWILIO_API_KEY_SECRET'; Kind='secret'; Guide='The matching API key secret shown when that key was created. This is not the account Auth Token.' },
            @{ Name='AZURE_OPENAI_API_KEY'; Kind='secret'; Guide='Optional existing Live Ninja Azure OpenAI key: Azure portal > the existing Azure OpenAI resource > Keys and Endpoint. Skip if using managed identity or the model/resource is not selected.' }
        )
        Write-Host ''
        Write-Host 'App / allowed origin: https://live.jeremy.ninja'
        Write-Host 'Privacy URL documented in existing setup: https://live.jeremy.ninja/privacy (route not verified).'
        Write-Host 'Existing web login callback: https://live.jeremy.ninja/auth/lwa/callback'
        Write-Host 'Twilio phone/media callbacks: not deployed yet. Do not use the login callback for Twilio.'
        Write-Host 'Twilio: https://console.twilio.com/'
        Write-Host 'API key guide: https://www.twilio.com/docs/iam/api-keys/keys-in-console'
        Write-Host 'Azure: https://portal.azure.com/'
        Write-Host 'Azure deployment uses OIDC. No Azure client secret or AWS key is requested.'
        Write-Host 'Other existing app credentials are outside this phone setup.'
        foreach ($item in $items) {
            $names = if ($item.Kind -eq 'secret') { $secretNames } else { $variableNames }
            $state = if ($names -contains $item.Name) { 'configured' } else { 'missing' }
            Write-Host ("{0}: {1} ({2})" -f $item.Name, $state, $item.Kind)
        }
        if ($Check) { Write-Host 'CHECK COMPLETE: no prompts or writes.'; return 0 }
        if ([Console]::IsInputRedirected -or [Console]::IsOutputRedirected) {
            Write-Host 'STOP: run the batch file in a local interactive cmd.exe window, without redirection.'
            return 1
        }
        Write-Host ''
        Write-Host 'Enter each missing value once. Empty input makes no change.'
        Write-Host 'The length and last four characters appear only here. Nothing is saved to a local file.'
        Write-Host 'Already configured entries are skipped unless --replace-existing was selected.'
        $failed = 0; $written = 0; $skipped = 0
        foreach ($item in $items) {
            Write-Host ''
            Write-Host $item.Guide
            $names = if ($item.Kind -eq 'secret') { $secretNames } else { $variableNames }
            $status = Set-PhoneValue $item.Name $item.Kind ($names -contains $item.Name) $Replace
            switch ($status) { 'failed' { $failed++ }; 'written' { $written++ }; 'skipped' { $skipped++ } }
        }
        Write-Host "Finished: $written written, $skipped skipped, $failed failed."
        Write-Host 'GitHub writes only. No calls, purchases, pushes, or deployments were started.'
        if ($failed -gt 0) { return 1 }
        return 0
    } catch {
        # Exception text can include process input. Do not print it.
        Write-Host 'STOP: setup failed. Check git/gh installation, GitHub access, and network. No automatic retries.'
        return 1
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    exit (Invoke-PhoneSetup -Check $CheckOnly.IsPresent -Replace $ReplaceExisting.IsPresent)
}
