# Regenerate the wake-word evaluation clips at 16 kHz mono with the text known BY CONSTRUCTION.
# The prior set mixed 22050 Hz files into a 16 kHz-only pipeline, which silently made a working
# model look broken. Every file here is named <voice>__<slug>.wav and its text is in clips.tsv.
Add-Type -AssemblyName System.Speech

$out = Join-Path $PSScriptRoot 'clips'
New-Item -ItemType Directory -Force -Path $out | Out-Null

# slug -> spoken text
$phrases = [ordered]@{
  # --- targets ---
  'tgt_hey_live_ninja'   = 'hey live ninja'
  'tgt_hey_sunshine'     = 'hey sunshine'
  'tgt_hey_automatica'   = 'hey automatica'
  'tgt_hey_jarvis'       = 'hey jarvis'

  # --- near misses for "hey live ninja": one position varied at a time ---
  'nm_hey_live_nina'     = 'hey live nina'
  'nm_hey_live_ginger'   = 'hey live ginger'
  'nm_hey_five_ninja'    = 'hey five ninja'
  'nm_hey_wild_ninja'    = 'hey wild ninja'
  'nm_okay_live_ninja'   = 'okay live ninja'
  'nm_the_live_ninja'    = 'the live ninja'
  'nm_hey_live_ninjas'   = 'hey live ninjas'
  'nm_live_ninja'        = 'live ninja'

  # --- near misses for the 2-word phrases ---
  'nm_hey_moonshine'     = 'hey moonshine'
  'nm_hey_sunday'        = 'hey sunday'
  'nm_hey_sunshade'      = 'hey sunshade'
  'nm_hey_automatic'     = 'hey automatic'
  'nm_hey_america'       = 'hey america'

  # --- generic "hey <something>" negatives ---
  'neg_hey_banana'       = 'hey banana'
  'neg_hey_jennifer'     = 'hey jennifer'
  'neg_hey_computer'     = 'hey computer'
  'neg_hey_marvin'       = 'hey marvin'
  'neg_hey_charlie'      = 'hey charlie'
  'neg_hey_there'        = 'hey there'
  'neg_hey_there_buddy'  = 'hey there buddy'
  'neg_hey_lima_beans'   = 'hey lima beans'

  # --- unrelated speech ---
  'neg_unrelated_1'      = 'could you turn the lights down a little in here'
  'neg_unrelated_2'      = 'the meeting got moved to half past four tomorrow'
}

$fmt = New-Object System.Speech.AudioFormat.SpeechAudioFormatInfo(16000, [System.Speech.AudioFormat.AudioBitsPerSample]::Sixteen, [System.Speech.AudioFormat.AudioChannel]::Mono)

$tsv = Join-Path $PSScriptRoot 'clips.tsv'
"file`tvoice`tslug`ttext" | Set-Content -Path $tsv -Encoding utf8

$synth = New-Object System.Speech.Synthesis.SpeechSynthesizer
foreach ($v in $synth.GetInstalledVoices()) {
  $vname = $v.VoiceInfo.Name
  $short = ($vname -replace 'Microsoft ', '' -replace ' Desktop', '').ToLower()
  foreach ($slug in $phrases.Keys) {
    $text = $phrases[$slug]
    $file = Join-Path $out "$short`__$slug.wav"
    $s = New-Object System.Speech.Synthesis.SpeechSynthesizer
    $s.SelectVoice($vname)
    $s.SetOutputToWaveFile($file, $fmt)
    $s.Speak($text)
    $s.Dispose()
    "$short`__$slug.wav`t$short`t$slug`t$text" | Add-Content -Path $tsv -Encoding utf8
  }
}
$synth.Dispose()

Write-Host "wrote $((Get-ChildItem $out -Filter *.wav).Count) clips to $out"
