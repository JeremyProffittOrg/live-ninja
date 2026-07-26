package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func TestEffectiveSettingsAndSectionMutations(t *testing.T) {
	doc := DefaultSettings()
	doc["futureField"] = map[string]any{"kept": true}
	doc["voiceEngine"] = map[string]any{
		"default": "openai-realtime",
		"devices": map[string]any{"dev-1": "nova-sonic"},
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ApplySettingsSection(doc, SettingsSectionWakeWord, map[string]any{
		"wakeWord":    "computer",
		"wakeEngine":  "porcupine",
		"sensitivity": 0.8,
	}, []string{"dev-1"}, false, false, now))
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionVoiceEngine, map[string]any{
		"voiceEngine": map[string]any{"default": "gemini-flash-live", "devices": map[string]any{}},
		"geminiVoice": "Kore",
	}, []string{"dev-1"}, false, false, now))

	effective := EffectiveSettings(doc, "dev-1")
	assert.Equal(t, "computer", effective["wakeWord"])
	assert.Equal(t, "Kore", effective["geminiVoice"])
	voiceEngine := effective["voiceEngine"].(map[string]any)
	assert.Equal(t, "gemini-flash-live", voiceEngine["default"],
		"explicit section override must outrank the deprecated legacy pin")
	assert.Empty(t, voiceEngine["devices"], "effective views must not expose cross-host legacy pins")
	_, exposesOverrides := effective["deviceOverrides"]
	assert.False(t, exposesOverrides)
	assert.Equal(t, true, effective["futureField"].(map[string]any)["kept"])
	assert.False(t, DeviceSectionInherited(doc, "dev-1", SettingsSectionWakeWord))

	require.NoError(t, ApplySettingsSection(doc, SettingsSectionWakeWord, nil,
		[]string{"dev-1"}, false, true, now.Add(time.Minute)))
	assert.True(t, DeviceSectionInherited(doc, "dev-1", SettingsSectionWakeWord))
	assert.Equal(t, "hey-live-ninja", EffectiveSettings(doc, "dev-1")["wakeWord"])

	require.NoError(t, ApplySettingsSection(doc, SettingsSectionVoiceEngine, map[string]any{
		"voiceEngine": map[string]any{"default": "openai-realtime-mini", "devices": map[string]any{}},
		"geminiVoice": "",
	}, nil, true, false, now.Add(2*time.Minute)))
	assert.Equal(t, "openai-realtime-mini", EffectiveSettings(doc, "dev-1")["voiceEngine"].(map[string]any)["default"])
	assert.True(t, DeviceSectionInherited(doc, "dev-1", SettingsSectionVoiceEngine))
	overrides := doc["deviceOverrides"].(map[string]any)
	assert.Empty(t, overrides, "empty override entries must be pruned")
}

func TestApplySettingsSectionEnforcesDeviceCap(t *testing.T) {
	doc := DefaultSettings()
	overrides := doc["deviceOverrides"].(map[string]any)
	for i := 0; i < MaxDeviceOverrides; i++ {
		id := "device-" + time.Unix(int64(i), 0).UTC().Format("150405")
		overrides[id] = map[string]any{
			"sections": map[string]any{
				SettingsSectionPrivacy: map[string]any{"privacy": map[string]any{"storeTranscripts": true}},
			},
		}
	}
	err := ApplySettingsSection(doc, SettingsSectionWakeWord,
		map[string]any{"wakeWord": "computer"}, []string{"one-too-many"}, false, false, time.Now())
	require.Error(t, err)
	assert.Len(t, overrides, MaxDeviceOverrides)

	// Updating an existing entry at the cap is still allowed.
	var existing string
	for id := range overrides {
		existing = id
		break
	}
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionWakeWord,
		map[string]any{"wakeWord": "computer"}, []string{existing}, false, false, time.Now()))
}

func TestApplySettingsSectionPreservesUnknownSectionFields(t *testing.T) {
	doc := DefaultSettings()
	doc["deviceOverrides"] = map[string]any{
		"device-1": map[string]any{
			"sections": map[string]any{
				SettingsSectionWakeWord: map[string]any{
					"wakeWord":         "old",
					"futureWakeTuning": map[string]any{"mode": "adaptive"},
				},
			},
		},
	}
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionWakeWord,
		map[string]any{"wakeWord": "computer"}, []string{"device-1"}, false, false, time.Now()))
	entry := doc["deviceOverrides"].(map[string]any)["device-1"].(map[string]any)
	section := entry["sections"].(map[string]any)[SettingsSectionWakeWord].(map[string]any)
	assert.Equal(t, "computer", section["wakeWord"])
	assert.Equal(t, "adaptive", section["futureWakeTuning"].(map[string]any)["mode"])

	require.NoError(t, ApplySettingsSection(doc, SettingsSectionWakeWord,
		nil, []string{"device-1"}, false, true, time.Now()))
	entry = doc["deviceOverrides"].(map[string]any)["device-1"].(map[string]any)
	section = entry["sections"].(map[string]any)[SettingsSectionWakeWord].(map[string]any)
	assert.NotContains(t, section, "wakeWord")
	assert.Equal(t, "adaptive", section["futureWakeTuning"].(map[string]any)["mode"],
		"an older inherit must not erase additive keys written by a newer client")

	require.NoError(t, ApplySettingsSection(doc, SettingsSectionWakeWord,
		map[string]any{"wakeWord": "computer"}, []string{"device-1"}, false, false, time.Now()))
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionWakeWord,
		map[string]any{
			"wakeWord": "everyone", "wakeEngine": "openwakeword", "sensitivity": 0.5,
		}, nil, true, false, time.Now()))
	entry = doc["deviceOverrides"].(map[string]any)["device-1"].(map[string]any)
	section = entry["sections"].(map[string]any)[SettingsSectionWakeWord].(map[string]any)
	assert.NotContains(t, section, "wakeWord")
	assert.Equal(t, "adaptive", section["futureWakeTuning"].(map[string]any)["mode"],
		"an older apply-all must not erase additive keys written by a newer client")
}

func TestApplySettingsSectionPreservesNestedUnknownFields(t *testing.T) {
	doc := DefaultSettings()
	doc["deviceOverrides"] = map[string]any{
		"device-1": map[string]any{
			"sections": map[string]any{
				SettingsSectionPrivacy: map[string]any{
					"privacy": map[string]any{
						"storeTranscripts":  false,
						"futurePrivacyMode": "local-only",
					},
				},
				SettingsSectionPersona: map[string]any{
					"persona": map[string]any{
						"presetId":     "custom",
						"futureAvatar": "ninja",
					},
					"personaPrefs": map[string]any{
						"custom": map[string]any{
							"voice":        "cedar",
							"futureTimbre": "warm",
						},
					},
				},
			},
		},
	}

	require.NoError(t, ApplySettingsSection(doc, SettingsSectionPrivacy,
		map[string]any{"privacy": map[string]any{
			"storeAudio": false, "storeTranscripts": true, "retentionDays": 30,
		}}, nil, true, false, time.Now()))
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionPersona,
		map[string]any{
			"persona":      map[string]any{"presetId": "default", "systemInstructions": nil},
			"voice":        "cedar",
			"voiceAccent":  "",
			"personaPrefs": map[string]any{},
		}, nil, true, false, time.Now()))

	sections := doc["deviceOverrides"].(map[string]any)["device-1"].(map[string]any)["sections"].(map[string]any)
	privacy := sections[SettingsSectionPrivacy].(map[string]any)["privacy"].(map[string]any)
	assert.NotContains(t, privacy, "storeTranscripts")
	assert.Equal(t, "local-only", privacy["futurePrivacyMode"])
	persona := sections[SettingsSectionPersona].(map[string]any)
	assert.Equal(t, "ninja", persona["persona"].(map[string]any)["futureAvatar"])
	assert.Equal(t, "warm",
		persona["personaPrefs"].(map[string]any)["custom"].(map[string]any)["futureTimbre"])
}

func TestClientDeviceLifecycleAndSessionRecovery(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	session := mkSession("session-1", "user-1", "family-1", "hash")
	require.NoError(t, st.CreateSession(ctx, session))

	deviceID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2cf"
	device, err := st.UpsertClientDevice(ctx, &Device{
		DeviceID: deviceID, UserID: "user-1", Name: "Chrome on Windows",
		Surface: SurfaceWeb, FamilyID: "family-1",
		Metadata:     map[string]string{"browser": "Chrome", "platform": "Windows"},
		Capabilities: []string{"wakeWord", "privacy"},
	})
	require.NoError(t, err)
	assert.Equal(t, DeviceStatusActive, device.Status)
	require.NoError(t, st.BindClientSessionDevice(ctx, "user-1", "session-1", deviceID))

	renamed, err := st.RenameDevice(ctx, "user-1", deviceID, "Office PC")
	require.NoError(t, err)
	assert.Equal(t, "Office PC", renamed.Name)
	assert.True(t, renamed.NameCustomized)

	refreshed, err := st.UpsertClientDevice(ctx, &Device{
		DeviceID: deviceID, UserID: "user-1", Name: "Edge on Windows",
		Surface: SurfaceWeb, FamilyID: "family-2",
	})
	require.NoError(t, err)
	assert.Equal(t, "Office PC", refreshed.Name, "inferred names must not replace a user rename")
	assert.Equal(t, "family-2", refreshed.FamilyID, "re-login must attach the current refresh family")

	newDeviceID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2d0"
	_, err = st.UpsertClientDevice(ctx, &Device{
		DeviceID: newDeviceID, UserID: "user-1", Name: "Fresh install", Surface: SurfaceWeb,
	})
	require.NoError(t, err)
	require.NoError(t, st.BindClientSessionDevice(ctx, "user-1", "session-1", newDeviceID),
		"web/Android sessions may recover after local device identity storage is cleared")
	gotSession, err := st.GetSessionByID(ctx, "session-1")
	require.NoError(t, err)
	assert.Equal(t, newDeviceID, gotSession.DeviceID)

	_, err = st.UpsertClientDevice(ctx, &Device{
		DeviceID: deviceID, UserID: "other-user", Name: "Stolen", Surface: SurfaceWeb,
	})
	require.ErrorIs(t, err, ErrDeviceOwnership)
}

func TestGetProfileForDeviceUsesAboutYouOverride(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	doc := DefaultSettings()
	doc["profile"].(map[string]any)["displayName"] = "Account Name"
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionAboutYou, map[string]any{
		"profile": map[string]any{
			"displayName": "Kitchen Name",
			"units":       UnitsMetric,
			"notes":       []any{},
		},
	}, []string{"device-1"}, false, false, time.Now()))
	_, err := st.PutSettings(ctx, "user-1", doc, 1)
	require.NoError(t, err)

	account, err := st.GetProfile(ctx, "user-1")
	require.NoError(t, err)
	device, err := st.GetProfileForDevice(ctx, "user-1", "device-1")
	require.NoError(t, err)
	assert.Equal(t, "Account Name", account.DisplayName)
	assert.Equal(t, "Kitchen Name", device.DisplayName)
	assert.Equal(t, UnitsMetric, device.Units)
}

func TestRevokeSessionsForDeviceCrossesRefreshFamilies(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	for _, session := range []*Session{
		mkSession("s1", "user-1", "family-1", "hash-1"),
		mkSession("s2", "user-1", "family-2", "hash-2"),
		mkSession("s3", "user-1", "family-3", "hash-3"),
	} {
		session.DeviceID = "device-1"
		if session.SessionID == "s3" {
			session.DeviceID = "device-2"
		}
		require.NoError(t, st.CreateSession(ctx, session))
	}

	require.NoError(t, st.RevokeSessionsForDevice(ctx, "user-1", "device-1"))
	for _, sessionID := range []string{"s1", "s2"} {
		session, err := st.GetSessionForUser(ctx, "user-1", sessionID)
		require.NoError(t, err)
		assert.Nil(t, session)
	}
	remaining, err := st.GetSessionForUser(ctx, "user-1", "s3")
	require.NoError(t, err)
	require.NotNil(t, remaining)
	assert.Equal(t, "device-2", remaining.DeviceID)
}

type consistencyRecordingDynamo struct {
	*testutil.FakeDynamo
	settingsGetConsistent  bool
	sessionQueryConsistent bool
}

func (r *consistencyRecordingDynamo) GetItem(ctx context.Context, in *dynamodb.GetItemInput,
	optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	r.settingsGetConsistent = in.ConsistentRead != nil && *in.ConsistentRead
	return r.FakeDynamo.GetItem(ctx, in, optFns...)
}

func (r *consistencyRecordingDynamo) Query(ctx context.Context, in *dynamodb.QueryInput,
	optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if in.IndexName == nil {
		r.sessionQueryConsistent = in.ConsistentRead != nil && *in.ConsistentRead
	}
	return r.FakeDynamo.Query(ctx, in, optFns...)
}

func TestSettingsAndSessionRevocationReadsAreStronglyConsistent(t *testing.T) {
	ctx := context.Background()
	recorder := &consistencyRecordingDynamo{FakeDynamo: testutil.NewFakeDynamo()}
	st := NewWithClient(recorder, "live-ninja-test")

	_, err := st.GetSettings(ctx, "user-1")
	require.NoError(t, err)
	assert.True(t, recorder.settingsGetConsistent)

	session := mkSession("session-1", "user-1", "family-1", "hash")
	session.DeviceID = "device-1"
	require.NoError(t, st.CreateSession(ctx, session))
	require.NoError(t, st.RevokeSessionsForDevice(ctx, "user-1", "device-1"))
	assert.True(t, recorder.sessionQueryConsistent)
}

func TestClientSessionCannotRebindAwayFromRevokedDevice(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	oldID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2cf"
	newID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2d0"
	for _, deviceID := range []string{oldID, newID} {
		_, err := st.UpsertClientDevice(ctx, &Device{
			DeviceID: deviceID, UserID: "user-1", Name: "Browser", Surface: SurfaceWeb,
		})
		require.NoError(t, err)
	}

	bound := mkSession("bound", "user-1", "family-bound", "hash-bound")
	bound.DeviceID = oldID
	require.NoError(t, st.CreateSession(ctx, bound))
	require.NoError(t, st.RevokeDevice(ctx, oldID))
	require.ErrorIs(t,
		st.BindClientSessionDevice(ctx, "user-1", "bound", newID),
		ErrDeviceRevoked)
	stillBound, err := st.GetSessionForUser(ctx, "user-1", "bound")
	require.NoError(t, err)
	require.NotNil(t, stillBound)
	assert.Equal(t, oldID, stillBound.DeviceID)

	blank := mkSession("blank", "user-1", "family-blank", "hash-blank")
	require.NoError(t, st.CreateSession(ctx, blank))
	require.NoError(t, st.BindClientSessionDevice(ctx, "user-1", "blank", newID),
		"a newly authenticated blank session may bind an active rotated identity")
	rebound, err := st.GetSessionForUser(ctx, "user-1", "blank")
	require.NoError(t, err)
	require.NotNil(t, rebound)
	assert.Equal(t, newID, rebound.DeviceID)
}

type revokeBeforeTransactionDynamo struct {
	*testutil.FakeDynamo
	revokeStore *Store
	deviceID    string
	once        sync.Once
}

func (r *revokeBeforeTransactionDynamo) TransactWriteItems(ctx context.Context,
	in *dynamodb.TransactWriteItemsInput,
	optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	r.once.Do(func() {
		if err := r.revokeStore.RevokeDevice(ctx, r.deviceID); err != nil {
			panic(err)
		}
	})
	return r.FakeDynamo.TransactWriteItems(ctx, in, optFns...)
}

func TestClientSessionRebindLosesRaceToDeviceRevoke(t *testing.T) {
	ctx := context.Background()
	fake := testutil.NewFakeDynamo()
	direct := NewWithClient(fake, "live-ninja-test")
	oldID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2cf"
	newID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2d0"
	for _, deviceID := range []string{oldID, newID} {
		_, err := direct.UpsertClientDevice(ctx, &Device{
			DeviceID: deviceID, UserID: "user-1", Name: "Browser", Surface: SurfaceWeb,
		})
		require.NoError(t, err)
	}
	session := mkSession("session-1", "user-1", "family-1", "hash")
	session.DeviceID = oldID
	require.NoError(t, direct.CreateSession(ctx, session))

	racing := NewWithClient(&revokeBeforeTransactionDynamo{
		FakeDynamo: fake, revokeStore: direct, deviceID: oldID,
	}, "live-ninja-test")
	require.ErrorIs(t,
		racing.BindClientSessionDevice(ctx, "user-1", "session-1", newID),
		ErrDeviceRevoked)
	fresh, err := direct.GetSessionForUser(ctx, "user-1", "session-1")
	require.NoError(t, err)
	require.NotNil(t, fresh)
	assert.Equal(t, oldID, fresh.DeviceID,
		"the active-device condition must cancel the session update atomically")
}

type renameRaceDynamo struct {
	*testutil.FakeDynamo
	renameStore *Store
	deviceID    string
	once        sync.Once
}

func (r *renameRaceDynamo) PutItem(ctx context.Context, in *dynamodb.PutItemInput,
	optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if in.ConditionExpression != nil && strings.Contains(*in.ConditionExpression, "#name") {
		r.once.Do(func() {
			_, err := r.renameStore.RenameDevice(ctx, "user-1", r.deviceID, "Renamed during refresh")
			if err != nil {
				panic(err)
			}
		})
	}
	return r.FakeDynamo.PutItem(ctx, in, optFns...)
}

func TestUpsertClientDevicePreservesConcurrentRename(t *testing.T) {
	ctx := context.Background()
	fake := testutil.NewFakeDynamo()
	direct := NewWithClient(fake, "live-ninja-test")
	deviceID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2cf"
	_, err := direct.UpsertClientDevice(ctx, &Device{
		DeviceID: deviceID, UserID: "user-1", Name: "Chrome on Windows", Surface: SurfaceWeb,
	})
	require.NoError(t, err)

	racing := &renameRaceDynamo{FakeDynamo: fake, renameStore: direct, deviceID: deviceID}
	st := NewWithClient(racing, "live-ninja-test")
	refreshed, err := st.UpsertClientDevice(ctx, &Device{
		DeviceID: deviceID, UserID: "user-1", Name: "Edge on Windows", Surface: SurfaceWeb,
		Metadata: map[string]string{"browser": "Edge"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Renamed during refresh", refreshed.Name)
	assert.True(t, refreshed.NameCustomized)
	assert.Equal(t, "Edge", refreshed.Metadata["browser"])
}

func TestUpsertClientDeviceRejectsPairedAndSurfaceTransitions(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	pairedID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2cf"
	require.NoError(t, st.CreateDevice(ctx, &Device{
		DeviceID: pairedID, UserID: "user-1", Name: "Kitchen M5",
		Surface: SurfaceDevice, FamilyID: "family-device", ThingName: "ln-kitchen",
	}))
	_, err := st.UpsertClientDevice(ctx, &Device{
		DeviceID: pairedID, UserID: "user-1", Name: "Browser collision", Surface: SurfaceWeb,
	})
	require.ErrorIs(t, err, ErrDeviceBindingConflict)

	clientID := "019c8dc7-0a68-7bd8-93a0-a39deeb7e2d0"
	_, err = st.UpsertClientDevice(ctx, &Device{
		DeviceID: clientID, UserID: "user-1", Name: "Browser", Surface: SurfaceWeb,
	})
	require.NoError(t, err)
	_, err = st.UpsertClientDevice(ctx, &Device{
		DeviceID: clientID, UserID: "user-1", Name: "Android collision", Surface: SurfaceAndroid,
	})
	require.ErrorIs(t, err, ErrDeviceBindingConflict)
}

func TestSettingsSizeHeadroomAndRevokedOverridePurge(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	tooLarge := DefaultSettings()
	tooLarge["futureBlob"] = strings.Repeat("x", MaxSettingsSerializedBytes)
	_, err := st.PutSettings(ctx, "large-user", tooLarge, 1)
	require.ErrorIs(t, err, ErrSettingsTooLarge)

	doc := DefaultSettings()
	require.NoError(t, ApplySettingsSection(doc, SettingsSectionPrivacy,
		map[string]any{"privacy": map[string]any{"storeTranscripts": false}},
		[]string{"revoked-device"}, false, false, time.Now()))
	_, err = st.PutSettings(ctx, "user-1", doc, 1)
	require.NoError(t, err)
	purged, version, changed, err := st.PurgeDeviceSettingsOverride(ctx, "user-1", "revoked-device")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.EqualValues(t, 3, version)
	assert.NotContains(t, purged["deviceOverrides"].(map[string]any), "revoked-device")
}

func TestRevokePurgeWithoutOverrideInvalidatesInflightSettingsWrite(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// This is the exact race ordering: PATCH read version 1 while the target
	// was active, revoke then commits its settings barrier with no prior
	// override, and PATCH finally attempts to create the override from its
	// stale copy.
	stalePatchDoc, err := st.GetSettings(ctx, "user-1")
	require.NoError(t, err)
	barrierDoc, barrierVersion, committed, err := st.PurgeDeviceSettingsOverride(
		ctx, "user-1", "revoked-device",
	)
	require.NoError(t, err)
	assert.True(t, committed)
	assert.EqualValues(t, 2, barrierVersion)
	assert.EqualValues(t, 2, barrierDoc["version"])

	require.NoError(t, ApplySettingsSection(stalePatchDoc, SettingsSectionPrivacy,
		map[string]any{"privacy": map[string]any{"storeTranscripts": false}},
		[]string{"revoked-device"}, false, false, time.Now()))
	_, err = st.PutSettings(ctx, "user-1", stalePatchDoc, 1)
	require.ErrorIs(t, err, ErrVersionConflict,
		"the revoke barrier must defeat an in-flight PATCH with no prior override")

	latest, err := st.GetSettings(ctx, "user-1")
	require.NoError(t, err)
	assert.EqualValues(t, 2, latest["version"])
	assert.NotContains(t, latest["deviceOverrides"].(map[string]any), "revoked-device")
}
