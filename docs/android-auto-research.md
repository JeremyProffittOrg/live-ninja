# Android Auto feasibility and test research

Research date: 2026-10-15. Planning evidence only. No app was built, installed, deployed, or called. Live Ninja's current runtime behavior was not tested in this research.

## Platform boundary

Android Auto projects a phone app onto a compatible car display. Android Automotive OS (AAOS) runs apps installed on the car itself. The requested scope should target Android Auto, not promise AAOS support. Google's supported categories include calling, but no general-purpose AI assistant category. Calling currently appears as a labs category for Android Auto only. Inference: Live Ninja's real VoIP calling feature is the credible entry point; a generic assistant interface does not have a documented standalone category. Do not relabel assistant audio as music to bypass that boundary. [Android for Cars overview](https://developer.android.com/training/cars)

## Calling integration and release limits

Android Auto requires Jetpack Telecom integration for call answering and control, including remote callbacks. That integration must operate outside Android Auto too. An optional Car App Library experience can show call-related content through a CarAppService declaring androidx.car.app.category.CALLING. Android Auto replaces these templates with its own in-call view during a call.

Calling-enabled builds currently belong only on Google Play Internal Testing or Closed Testing tracks. Google explicitly says Open Testing and Production submissions containing calling support will be rejected. The guide also warns that additional Telecom requirements are forthcoming. Recheck these rules before implementation and release; do not promise a public Play launch. [Calling guide](https://developer.android.com/training/cars/communication/calling)

## Audio ownership and native call lifecycle

Core-Telecom requires MANAGE_OWN_CALLS and CallsManager registration. Map the real VoIP lifecycle to answer, active, inactive, and disconnect events. Handle remote actions from Bluetooth and Android Auto within five seconds.

For a Telecom-managed call, observe currentCallEndpoint, availableEndpoints, and isMuted; change routes with requestEndpointChange. Google explicitly warns against using AudioManager.setCommunicationDevice or startBluetoothSco while Telecom owns the call. The media stream must use STREAM_VOICE_CALL. Foreground call service and timely CallStyle notification are also required.

Design implication: distinguish standalone assistant audio from a Telecom-managed Twilio call, and ensure only one route controller owns a session. This is a proposed design constraint, not a verified Twilio integration. [Core-Telecom](https://developer.android.com/develop/connectivity/telecom/voip-app/telecom)

## Testing and distribution evidence

DHU (Desktop Head Unit) runs on Windows, macOS, and Linux and uses a connected Android phone. It supports microphone input, prerecorded voice input, microphone rejection, screen/input variations, and simulated driving state. These are useful automated/manual checks. The guide does not establish real vehicle Bluetooth microphone or acoustic performance. Inference: DHU success cannot prove the reported hands-free defect is fixed. [DHU testing](https://developer.android.com/training/cars/testing/dhu)

Real-vehicle testing needs installation from a trusted source, such as Google Play; Google provides Internal App Sharing and Internal Test Track for development. The Android Auto unknown-sources exception does not apply to Car App Library apps. A locally sideloaded APK is therefore not sufficient as the planned templated-app vehicle test distribution path. [Car testing](https://developer.android.com/training/cars/testing)

Google's calling criteria cover initiate/accept/decline/leave, call identity, mute, group participant information, and continuity when Android Auto connects during a call. Other criteria cover response times, short task flows, and safe instructions when phone interaction is necessary. Google warns against using a production APK for Android Auto prototyping because review can delay other device updates. [Car app quality](https://developer.android.com/docs/quality-guidelines/car-app-quality)

## Proposed acceptance matrix (project recommendations, not measured results)

- Run standalone assistant sessions with Bluetooth hands-free only, then wired and wireless Android Auto where the actual vehicle supports each.
- Run real inbound and outbound Twilio calls to a controlled consenting test endpoint. Verify car microphone uplink and car speaker downlink separately; speaker playback alone is insufficient.
- Repeat app-controlled calls with assistant silent listening, assistant takeover, and return to the user. Verify mute prevents unintended uplink and the assistant does not speak in listen-only mode.
- Verify answer/reject/end from vehicle controls; reconnect during a live call; switch car/handset/speaker; unplug projection; disable Bluetooth; lock the phone; background the app; interrupt with a cellular call; and recover from network loss.
- Include an older supported phone and a current Android phone. Record OS, app version, vehicle/head-unit model, wired/wireless mode, selected input/output devices, call identifiers, failures, and measured delays without credentials or raw private speech.
- Treat the five-hotel job as a later end-to-end job test after controlled calls pass. It cannot substitute for car audio checks. Use five randomly selected California hotels under the locked selection policy, call count, retry ceiling, spending grants, recording permission, and stop condition.
- Require real parked-vehicle evidence for Bluetooth acceptance. Do not mark unrun hardware or paid-call cases passed. Keep missing vehicle, phone, Play distribution, and controlled endpoint access explicit in the plan.

## Unverified constraints

Eligibility of a direct AI-conversation surface, Google approval of Live Ninja's exact calling experience, Twilio-to-Telecom audio integration, actual vehicle support, and installed phone compatibility remain unverified. The existing Bluetooth defect's root cause requires repository inspection and device reproduction; this research does not diagnose it.


## Bluetooth source inspection and USB control evidence

Checked 2026-10-15 by the primary agent. This is source inspection plus device access verification, not a reproduced car-audio failure.

- Windows Get-PnpDevice -PresentOnly, filtered to USB entries containing serial 4633424442303098, returned Status OK, FriendlyName SAMSUNG Mobile USB Composite Device, Class USB.
- C:\Users\Jeremy\AppData\Local\Android\Sdk\platform-tools\adb.exe devices -l reports 4633424442303098 as device, model SM_G965U. A second connected device is tablet R52XC06P9KJ; all phone commands explicitly select the phone serial.
- getprop ro.product.model returned SM-G965U (Galaxy S9 family/S9+ model); ro.build.version.release returned 10; ro.build.version.sdk returned 29.
- Live Ninja package metadata reports versionName=0.3.15 and versionCode=21. The local android/app/build.gradle.kts still declares 0.3.13 / 19. Establish the installed artifact's commit before claiming the inspected source is the exact installed binary.
- adb -s 4633424442303098 shell input keyevent KEYCODE_WAKEUP exited 0; dumpsys power reported mWakefulness=Awake.
- adb -s 4633424442303098 shell am start -W -n ninja.jeremy.liveninja/.MainActivity returned Status: ok, Activity: ninja.jeremy.liveninja/.MainActivity. dumpsys activity activities confirmed mResumedActivity for that activity. This proves basic shell/input/activity control, not car microphone capture.
- Android Auto package metadata includes 17.7.663654-release and the system stub 1.0.892210-stub. An installed package alone is not supported-head-unit evidence.

Google's current requirements list Android 11 or higher for wired car projection and Android 11 for wireless compatibility. The attached Android 10 phone is below that documented baseline. Keep it for the legacy Bluetooth regression; use a supported newer phone for current Android Auto acceptance. Do not upgrade, reset, or replace the S9 as part of research. [Android Auto requirements](https://support.google.com/androidauto/answer/6348019)

### Code facts, not a completed diagnosis

- android/app/src/main/java/ninja/jeremy/liveninja/realtime/WebRtcTransport.kt:493-531 sets MODE_IN_COMMUNICATION. API 31+ prefers SCO/wired/USB then speaker; API 29/30 sets isSpeakerphoneOn=true.
- GeminiLiveTransport.kt:952-977 repeats that routing policy.
- NovaBridgeTransport.kt:535-558 selects the built-in speaker even on API 31+ and also forces speaker on older versions. Do not enable a disabled engine just to broaden this task.
- android/app/src/main/AndroidManifest.xml declares MODIFY_AUDIO_SETTINGS, but no Bluetooth permissions. Determine permissions needed by the selected APIs instead of adding scanning/location permissions speculatively.
- SessionOrchestrator.kt requests communication audio focus. Existing tests cover orchestration and voice processing; reuse those seams.
- No car/head-unit connection or failing acoustic test was established in this session. Source defects are leads, not proof of the reported runtime cause.

For standalone assistant audio on API 29/30, investigate legacy HFP/SCO routing: SCO is asynchronous, must be observed reaching connected state, and must be released on failure/end. Cellular telephony takes precedence and the app cannot assume routing returns afterward. A2DP media playback is not proof of car microphone support. On API 31+, use available communication devices and route-change callbacks, check request success, and release selection at end. For a Telecom-managed phone call use Telecom endpoint APIs instead, as described above. [AudioManager API](https://developer.android.com/reference/android/media/AudioManager), [audio routing guide](https://developer.android.com/develop/connectivity/bluetooth/ble-audio/audio-manager), [Bluetooth permissions](https://developer.android.com/develop/connectivity/bluetooth/bt-permissions)

### Diagnosis gate for future implementation

First build and run a failing two-way audio check in the parked target car, with known app artifact, engine, phone, and head-unit version. Capture selected input/output and spoken test phrase at each end. Only then test ranked possibilities: the pre-31 forced-speaker path; missing SCO/route lifecycle; model transport/capture format divergence; focus/wake-mic contention. Change one variable at a time. The future fix must turn the same failing check green and preserve handset/headset behavior. Investigation and runtime test phases are deferred because the operator requested research/planning, not implementation or test calls now.
