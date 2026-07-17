# ln_iot — AWS IoT Core integration (M5Stack Tab5)

Owns everything MQTT/IoT on the device (plan.md M5 "IoT + OTA" workstream):
mTLS MQTT to the account ATS endpoint, fleet provisioning by claim, the
`config` named shadow ([contracts/shadow.md](../../../contracts/shadow.md)),
telemetry ([contracts/telemetry.schema.json](../../../contracts/telemetry.schema.json)),
control topics, and IoT-Jobs A/B OTA with rollback.

## Wiring (integrator / `main`)

```c
ln_iot_init();                       // after nvs_flash_init() + default event loop
ln_iot_register_rssi_provider(fn);   // ln_net: return esp_wifi_sta_get_ap_info().rssi
esp_event_handler_register(LN_IOT_EVENT, ESP_EVENT_ANY_ID, handler, NULL);
// ... once the C6 WiFi link has an IP:
ln_iot_start();                      // provisions first if needed, then connects
// on WiFi loss: ln_iot_stop(); on regain: ln_iot_start();
```

`ctrl` responsibilities against this component:
- Handle `LN_IOT_EVENT_CONFIG_DELTA`: apply the fields, persist its settings,
  then call `ln_iot_shadow_report()` with the **full** new reported state
  (including the delta's `settings_version`). At boot (once connected), report
  the current state once so the backend sees liveness.
- Call `ln_iot_set_app_state("idle"|"listening"|"thinking"|"speaking"|...)` on
  every ctrl state change (feeds the 30s heartbeat).
- Handle `LN_IOT_EVENT_CONTROL_DOWN` (pairing bind token etc.) — **free the
  `ln_iot_buf_t.data` pointer** after use.
- Emit voice-flow telemetry via `ln_iot_publish_telemetry()` (e.g.
  `wake_word_detected`, `barge_in`, `session_started`).

## Credentials & NVS layout (namespace `ln_iot`)

| Key | Type | Written by | Purpose |
|---|---|---|---|
| `endpoint` | str | pairing (`ln_iot_store_bootstrap`) | ATS data endpoint hostname |
| `claim_cert` / `claim_key` | blob PEM | pairing | Fleet-provisioning claim identity |
| `tmpl` | str | pairing (optional) | Provisioning template (default: Kconfig `LN_IOT_PROV_TEMPLATE`) |
| `op_cert` / `op_key` | blob PEM | ln_iot (provisioning) | Operational device identity; **key generated on-chip, never leaves the device** |
| `thing` | str | ln_iot (RegisterThing) | IoT Thing name == MQTT client id |
| `device_id` / `user_id` | str | pairing | Telemetry attribution (`deviceId`/`userId`) |
| `ota_job` / `ota_ver` | str | ln_iot (OTA) | In-flight OTA job closed out on next boot |
| `set_ver` | i32 | ln_iot (shadow) | Last reported `settingsVersion` (higher-version-wins) |

**Pairing contract addition:** the `GET /auth/device/pair/claim` response's
"provisioning claim (cert bootstrap material)" must carry `iotEndpoint`,
`claimCertificatePem`, `claimPrivateKeyPem`, `provisioningTemplate`, and
`deviceId`/`userId`; `ln_net` passes them to `ln_iot_store_bootstrap()`.
Alternative bootstrap for bench/HIL builds: bake a claim cert at build time by
setting env `LN_IOT_CLAIM_CERT`/`LN_IOT_CLAIM_KEY` (PEM file paths) before
`idf.py build` — used only when NVS holds no claim material. Never commit PEMs.

Cert privacy: PEMs are NVS **blobs** so they are covered once NVS encryption is
enabled (M5 security-hardening task; `nvs_keys` partition already exists).
Migrating the operational key into the DS peripheral is that same task's scope —
only `ln_iot_provision.c`'s keygen/persist step changes.

## Fleet provisioning by claim (CSR flow)

1. Generate EC P-256 keypair on device (mbedtls + HW RNG) + CSR (`CN=<serial>`).
2. Connect with the **claim** cert (client id `ln-claim-<serial>`).
3. `$aws/certificates/create-from-csr/json` → signed operational cert + ownership token.
4. `$aws/provisioning-templates/<tmpl>/provision/json` with `parameters.SerialNumber`
   (backend `deviceId`, falling back to the eFuse MAC) → `thingName`.
5. Persist op cert/key/thing to NVS, drop the claim session, connect as the Thing.

The template must scope the per-device policy with
`${iot:Connection.Thing.ThingName}` (plan.md M5).

## Topics

- `liveninja/<thing>/telemetry` — heartbeat (30s: fw version, rssi, free heap,
  app state) + all telemetry events, QoS0 enqueue (never blocks callers).
- `liveninja/<thing>/control/up|down` — QoS1 app control channel.
- `$aws/things/<thing>/shadow/name/config/...` — settings sync per shadow.md
  (delta applied only when `desired.settingsVersion` > last reported; stale
  deltas re-publish the cached reported doc).
- `$aws/things/<thing>/jobs/...` — OTA jobs.

## OTA (IoT Jobs, A/B, rollback)

Job document: `{"operation":"ota","url":"<https>","sha256":"<64 hex>","version":"x.y.z","force":false}`.

Download via `esp_https_ota` into the inactive slot → **SHA-256 read-back
verify against the job's pinned hash before the slot becomes bootable** →
persist jobId/version → reboot. The new image boots `PENDING_VERIFY`; the first
successful MQTT connect marks it valid (`esp_ota_mark_app_valid_cancel_rollback`)
and reports the job `SUCCEEDED` (+ `ota_completed`). No cloud check-in within
`LN_IOT_VERIFY_TIMEOUT_S` (default 300s) → `esp_ota_mark_app_invalid_rollback_and_reboot()`;
the rolled-back image then reports the job `FAILED` (+ `ota_rolled_back`).
Requires `CONFIG_BOOTLOADER_APP_ROLLBACK_ENABLE=y` (set in `sdkconfig.defaults`).

Anti-rollback: version downgrades are rejected app-side unless `force:true`.
**Hardware anti-rollback** (Secure Boot v2 + eFuse `secure_version`,
`CONFIG_BOOTLOADER_APP_ANTI_ROLLBACK`) is deliberately NOT enabled here — it
burns irreversible eFuses and belongs to the M5 "flash encryption + Secure
Boot v2" hardening task, executed on real hardware after Secure Boot signing
keys exist. When enabled, bump `secure_version` in tandem with release tagging.
