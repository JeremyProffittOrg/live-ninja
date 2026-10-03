#!/usr/bin/env bash
# Called only inside the existing Linux emulator action, after its isolated emulator boots.
set -uo pipefail
diagnostics="app/build/outputs/instrumentation-diagnostics"
mkdir -p "$diagnostics" || exit 1
adb logcat -v brief -s LNTestProgress:I '*:S' &
progress_pid=$!
cleanup_progress() {
  kill -TERM "$progress_pid" 2>/dev/null || return 0
  for attempt in 1 2; do
    kill -0 "$progress_pid" 2>/dev/null || return 0
    sleep 1
  done
  kill -KILL "$progress_pid" 2>/dev/null || true
}
trap cleanup_progress EXIT

# Leave time inside the 30-minute job limit to collect evidence and upload it.
# timeout returns nonzero on a stall; never convert a failed run into success.
timeout --signal=TERM --kill-after=10s 15m ./gradlew connectedDebugAndroidTest --no-daemon \
  -Pandroid.testInstrumentationRunnerArguments.listener=ninja.jeremy.liveninja.TestProgressListener
test_status=$?
printf 'Instrumentation command exit: %s\n' "$test_status" | tee "$diagnostics/exit-status.txt"

timeout --signal=TERM --kill-after=2s 10s adb shell run-as ninja.jeremy.liveninja cat cache/test-progress.log > "$diagnostics/test-progress.log" 2>&1 || true
timeout --signal=TERM --kill-after=2s 10s adb logcat -d -v threadtime -t 2000 -s LNTestProgress:I AndroidRuntime:E ActivityManager:W '*:S' > "$diagnostics/logcat.txt" 2>&1 || true
timeout --signal=TERM --kill-after=2s 10s adb shell dumpsys activity activities > "$diagnostics/activities.txt" 2>&1 || true
timeout --signal=TERM --kill-after=2s 10s adb shell dumpsys window windows > "$diagnostics/windows.txt" 2>&1 || true
timeout --signal=TERM --kill-after=2s 10s adb shell dumpsys meminfo ninja.jeremy.liveninja > "$diagnostics/app-memory.txt" 2>&1 || true
timeout --signal=TERM --kill-after=2s 10s adb shell cat /proc/meminfo > "$diagnostics/emulator-memory.txt" 2>&1 || true
if [ "$test_status" -ne 0 ]; then
  timeout --signal=TERM --kill-after=2s 10s adb shell dumpsys activity lastanr > "$diagnostics/last-anr.txt" 2>&1 || true
  timeout --signal=TERM --kill-after=2s 10s adb exec-out screencap -p > "$diagnostics/screen.png" 2> "$diagnostics/screen-error.txt" || true
fi
exit "$test_status"
