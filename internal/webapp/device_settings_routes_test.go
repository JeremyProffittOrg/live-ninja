package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

const (
	testDeviceOne = "019c8dc7-0a68-7bd8-93a0-a39deeb7e2cf"
	testDeviceTwo = "019c8dc7-0a68-7bd8-93a0-a39deeb7e2d0"
	testDeviceOld = "019c8dc7-0a68-7bd8-93a0-a39deeb7e2d1"
)

func newDeviceSettingsApp(t *testing.T, sessionDeviceID string) (*fiber.App, *Deps, *store.Store) {
	t.Helper()
	fake := testutil.NewFakeDynamo()
	st := store.NewWithClient(fake, "live-ninja-test")
	now := time.Now()
	require.NoError(t, st.CreateSession(t.Context(), &store.Session{
		SessionID: "session-1", UserID: "user-1", FamilyID: "family-1",
		Surface: store.SurfaceWeb, DeviceID: sessionDeviceID, RefreshHash: "hash",
		CreatedAt: now.Unix(), LastUsedAt: now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(), TTL: now.Add(time.Hour).Unix(),
	}))
	deps := &Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "user-1")
		c.Locals(localSessionID, "session-1")
		c.Locals(localSurface, store.SurfaceWeb)
		c.Locals(localDeviceID, c.Get("X-Test-JWT-DID"))
		return c.Next()
	})
	app.Put("/api/v1/devices/current", handleRegisterCurrentDevice(deps))
	app.Get("/api/v1/devices", handleListDevices(deps))
	app.Patch("/api/v1/devices/:id", handleRenameDevice(deps))
	app.Get("/api/v1/settings", handleGetSettings(deps))
	app.Get("/api/v1/settings/sections/:section", handleGetSettingsSection(deps))
	app.Patch("/api/v1/settings/sections/:section", handlePatchSettingsSection(deps))
	return app, deps, st
}

func deviceSettingsRequest(t *testing.T, app *fiber.App, method, path, deviceID, jwtDeviceID string,
	body any) (*http.Response, map[string]any) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if deviceID != "" {
		req.Header.Set(deviceIDHeaderName, deviceID)
	}
	if jwtDeviceID != "" {
		req.Header.Set("X-Test-JWT-DID", jwtDeviceID)
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var out map[string]any
	if len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	}
	return resp, out
}

func seedClientDevice(t *testing.T, st *store.Store, userID, deviceID, name string) {
	t.Helper()
	_, err := st.UpsertClientDevice(t.Context(), &store.Device{
		DeviceID: deviceID, UserID: userID, Name: name, Surface: store.SurfaceWeb,
		Metadata:     map[string]string{"browser": "Chrome", "platform": "Windows"},
		Capabilities: store.SettingsSectionIDs(),
	})
	require.NoError(t, err)
}

type laggingDeviceIndexDynamo struct {
	*testutil.FakeDynamo
}

func (d *laggingDeviceIndexDynamo) Query(ctx context.Context, in *dynamodb.QueryInput,
	optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if in.IndexName != nil && *in.IndexName == "GSI2" {
		if key, ok := in.ExpressionAttributeValues[":pk"].(*types.AttributeValueMemberS); ok && key.Value == "DEVSEEN" {
			return &dynamodb.QueryOutput{}, nil
		}
	}
	return d.FakeDynamo.Query(ctx, in, optFns...)
}

func TestCurrentDeviceSurvivesDeviceIndexLag(t *testing.T) {
	fake := &laggingDeviceIndexDynamo{FakeDynamo: testutil.NewFakeDynamo()}
	st := store.NewWithClient(fake, "live-ninja-test")
	now := time.Now()
	require.NoError(t, st.CreateSession(t.Context(), &store.Session{
		SessionID: "session-1", UserID: "user-1", FamilyID: "family-1",
		Surface: store.SurfaceWeb, DeviceID: testDeviceOne, RefreshHash: "hash",
		CreatedAt: now.Unix(), LastUsedAt: now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(), TTL: now.Add(time.Hour).Unix(),
	}))
	seedClientDevice(t, st, "user-1", testDeviceOne, "Just registered browser")

	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "user-1")
		c.Locals(localSessionID, "session-1")
		c.Locals(localSurface, store.SurfaceWeb)
		return c.Next()
	})
	app.Get("/api/v1/devices", handleListDevices(deps))
	app.Get("/api/v1/settings/sections/:section", handleGetSettingsSection(deps))

	resp, out := deviceSettingsRequest(t, app, http.MethodGet, "/api/v1/devices",
		testDeviceOne, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	devices := out["devices"].([]any)
	require.Len(t, devices, 1)
	assert.Equal(t, true, devices[0].(map[string]any)["isCurrent"])

	resp, out = deviceSettingsRequest(t, app, http.MethodGet,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	rows := out["devices"].([]any)
	require.Len(t, rows, 1)
	assert.Equal(t, true, rows[0].(map[string]any)["isCurrent"])
}

func TestRegisterListAndRenameCurrentDevice(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")

	resp, out := deviceSettingsRequest(t, app, http.MethodPut, "/api/v1/devices/current",
		testDeviceOne, "", map[string]any{
			"deviceId": testDeviceOne, "suggestedName": "Chrome on Windows",
			"metadata": map[string]any{
				"surface": "web", "browser": "Chrome", "platform": "Windows",
				"unsafeFingerprint": "discard-me",
			},
			"capabilities": []string{"wakeWord", "privacy"},
		})
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	device := out["device"].(map[string]any)
	assert.Equal(t, "Chrome on Windows", device["name"])
	assert.Equal(t, true, device["isCurrent"])
	assert.NotEmpty(t, device["lastSeenAt"])
	assert.NotContains(t, device["metadata"].(map[string]any), "unsafeFingerprint")

	session, err := st.GetSessionByID(t.Context(), "session-1")
	require.NoError(t, err)
	assert.Equal(t, testDeviceOne, session.DeviceID)

	resp, out = deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/devices/"+testDeviceOne, testDeviceOne, "", map[string]any{"name": "Office PC"})
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	assert.Equal(t, "Office PC", out["device"].(map[string]any)["name"])

	// A background metadata refresh must retain the user's chosen name.
	resp, out = deviceSettingsRequest(t, app, http.MethodPut, "/api/v1/devices/current",
		testDeviceOne, "", map[string]any{"suggestedName": "Edge on Windows"})
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	assert.Equal(t, "Office PC", out["device"].(map[string]any)["name"])

	resp, out = deviceSettingsRequest(t, app, http.MethodGet, "/api/v1/devices",
		testDeviceOne, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	devices := out["devices"].([]any)
	require.Len(t, devices, 1)
	assert.Equal(t, true, devices[0].(map[string]any)["isCurrent"])
}

func TestRegisterDeviceOwnershipCollisionIsStableConflict(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")
	seedClientDevice(t, st, "other-user", testDeviceOne, "Other account browser")

	resp, out := deviceSettingsRequest(t, app, http.MethodPut, "/api/v1/devices/current",
		testDeviceOne, "", map[string]any{"suggestedName": "Chrome on Windows"})
	require.Equal(t, http.StatusConflict, resp.StatusCode, out)
	errBody, ok := out["error"].(map[string]any)
	require.True(t, ok, out)
	assert.Equal(t, "device_conflict", errBody["code"])
}

func TestRegisterCannotRotateSessionAwayFromRevokedDevice(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, testDeviceOld)
	seedClientDevice(t, st, "user-1", testDeviceOld, "Revoked browser")
	require.NoError(t, st.RevokeDevice(t.Context(), testDeviceOld))

	for _, rotatedID := range []string{testDeviceOne, testDeviceTwo} {
		resp, out := deviceSettingsRequest(t, app, http.MethodPut, "/api/v1/devices/current",
			rotatedID, testDeviceOld, map[string]any{
				"deviceId": rotatedID, "suggestedName": "Replacement browser",
			})
		require.Equal(t, http.StatusConflict, resp.StatusCode, out)
		errBody, ok := out["error"].(map[string]any)
		require.True(t, ok, out)
		assert.Equal(t, "device_revoked", errBody["code"],
			"rotating the UUID must not let an old revoked session escape")
		replacement, err := st.GetDevice(t.Context(), rotatedID)
		require.NoError(t, err)
		assert.Nil(t, replacement,
			"a rejected stale-session rotation must not leave an active orphan device")
	}

	session, err := st.GetSessionForUser(t.Context(), "user-1", "session-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, testDeviceOld, session.DeviceID)
}

func TestBlankReauthenticatedSessionCanRotateRevokedStoredIdentity(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")
	seedClientDevice(t, st, "user-1", testDeviceOld, "Previously revoked install")
	require.NoError(t, st.RevokeDevice(t.Context(), testDeviceOld))

	resp, out := deviceSettingsRequest(t, app, http.MethodPut, "/api/v1/devices/current",
		testDeviceOld, "", map[string]any{
			"deviceId": testDeviceOld, "suggestedName": "Browser",
		})
	require.Equal(t, http.StatusConflict, resp.StatusCode, out)
	assert.Equal(t, "device_revoked", out["error"].(map[string]any)["code"])

	resp, out = deviceSettingsRequest(t, app, http.MethodPut, "/api/v1/devices/current",
		testDeviceOne, "", map[string]any{
			"deviceId": testDeviceOne, "suggestedName": "Browser",
		})
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	session, err := st.GetSessionForUser(t.Context(), "user-1", "session-1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, testDeviceOne, session.DeviceID)
}

func TestRegisterValidatesFinalInferredDeviceName(t *testing.T) {
	app, _, _ := newDeviceSettingsApp(t, "")
	for _, metadata := range []map[string]any{
		{"browser": strings.Repeat("b", 45), "platform": strings.Repeat("p", 45)},
		{"browser": "Bad\nBrowser", "platform": "Windows"},
		{"appVersion": "1.0\nspoofed"},
	} {
		resp, _ := deviceSettingsRequest(t, app, http.MethodPut, "/api/v1/devices/current",
			testDeviceOne, "", map[string]any{
				"deviceId": testDeviceOne, "metadata": metadata,
			})
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
}

type revokeBarrierDynamo struct {
	*testutil.FakeDynamo
	marked  chan struct{}
	release chan struct{}
}

func (d *revokeBarrierDynamo) UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput,
	optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if in.UpdateExpression != nil && strings.Contains(*in.UpdateExpression, "#st = :revoked") {
		out, err := d.FakeDynamo.UpdateItem(ctx, in, optFns...)
		if err == nil {
			close(d.marked)
			<-d.release
		}
		return out, err
	}
	return d.FakeDynamo.UpdateItem(ctx, in, optFns...)
}

func TestRevokeMarksDeviceBeforeSweepingSessions(t *testing.T) {
	fake := &revokeBarrierDynamo{
		FakeDynamo: testutil.NewFakeDynamo(),
		marked:     make(chan struct{}),
		release:    make(chan struct{}),
	}
	st := store.NewWithClient(fake, "live-ninja-test")
	_, err := st.UpsertClientDevice(t.Context(), &store.Device{
		DeviceID: testDeviceOld, UserID: "user-1", Name: "Old browser",
		Surface: store.SurfaceWeb, FamilyID: "family-1",
	})
	require.NoError(t, err)
	seedClientDevice(t, st, "user-1", testDeviceOne, "Replacement browser")
	now := time.Now()
	require.NoError(t, st.CreateSession(t.Context(), &store.Session{
		SessionID: "session-1", UserID: "user-1", FamilyID: "family-1",
		Surface: store.SurfaceWeb, DeviceID: testDeviceOld, RefreshHash: "hash",
		CreatedAt: now.Unix(), LastUsedAt: now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(), TTL: now.Add(time.Hour).Unix(),
	}))

	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "user-1")
		return c.Next()
	})
	app.Delete("/api/v1/devices/:id", handleRevokeDevice(deps))

	type appResult struct {
		response *http.Response
		err      error
	}
	result := make(chan appResult, 1)
	go func() {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/devices/"+testDeviceOld, nil)
		resp, requestErr := app.Test(req)
		result <- appResult{response: resp, err: requestErr}
	}()

	select {
	case <-fake.marked:
	case <-time.After(2 * time.Second):
		t.Fatal("revoke did not reach the status barrier")
	}
	bindErr := st.BindClientSessionDevice(t.Context(), "user-1", "session-1", testDeviceOne)
	close(fake.release)
	require.ErrorIs(t, bindErr,
		store.ErrDeviceRevoked,
		"a rebind after the status barrier must fail before the session sweep")

	completed := <-result
	require.NoError(t, completed.err)
	require.Equal(t, http.StatusNoContent, completed.response.StatusCode)
	session, err := st.GetSessionForUser(t.Context(), "user-1", "session-1")
	require.NoError(t, err)
	assert.Nil(t, session, "the post-barrier strong sweep must remove the old session")
}

func TestSettingsSectionCurrentAllEffectiveAndConflict(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")
	interceptShadowPublish(t)
	seedClientDevice(t, st, "user-1", testDeviceOne, "Office PC")
	seedClientDevice(t, st, "user-1", testDeviceTwo, "Kitchen display")
	require.NoError(t, st.BindClientSessionDevice(t.Context(), "user-1", "session-1", testDeviceOne))

	resp, envelope := deviceSettingsRequest(t, app, http.MethodGet,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, envelope)
	assert.Equal(t, float64(1), envelope["version"])
	require.Len(t, envelope["devices"].([]any), 2)
	first := envelope["devices"].([]any)[0].(map[string]any)
	assert.NotNil(t, first["metadata"])
	assert.NotNil(t, first["capabilities"])

	setCurrent := map[string]any{
		"version": 1, "operation": "set",
		"target": map[string]any{"mode": "current", "deviceIds": []string{}},
		"settings": map[string]any{
			"wakeWord": "computer", "wakeEngine": "porcupine", "sensitivity": 0.75,
		},
	}
	resp, envelope = deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", setCurrent)
	require.Equal(t, http.StatusOK, resp.StatusCode, envelope)
	assert.Equal(t, float64(2), envelope["version"])

	resp, effective := deviceSettingsRequest(t, app, http.MethodGet,
		"/api/v1/settings?effective=true", testDeviceOne, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, effective)
	assert.Equal(t, "computer", effective["wakeWord"])
	assert.NotContains(t, effective, "deviceOverrides")

	resp, canonical := deviceSettingsRequest(t, app, http.MethodGet,
		"/api/v1/settings", testDeviceOne, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, canonical)
	assert.Equal(t, "hey-live-ninja", canonical["wakeWord"], "current-only set must not change defaults")

	applyAll := map[string]any{
		"version": 2, "operation": "set",
		"target": map[string]any{"mode": "all", "deviceIds": []string{}},
		"settings": map[string]any{
			"wakeWord": "ninja", "wakeEngine": "openwakeword", "sensitivity": 0.4,
		},
	}
	resp, envelope = deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", applyAll)
	require.Equal(t, http.StatusOK, resp.StatusCode, envelope)
	assert.Equal(t, float64(3), envelope["version"])
	for _, raw := range envelope["devices"].([]any) {
		row := raw.(map[string]any)
		assert.Equal(t, true, row["inherited"])
		assert.Equal(t, "ninja", row["settings"].(map[string]any)["wakeWord"])
	}

	resp, _ = deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", setCurrent)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "stale section writes must return 409")
}

func TestSettingsSectionNormalizesLegacyDeviceCollections(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")
	_, err := st.UpsertClientDevice(t.Context(), &store.Device{
		DeviceID: testDeviceOne,
		UserID:   "user-1",
		Name:     "Legacy display",
		Surface:  store.SurfaceWeb,
	})
	require.NoError(t, err)
	require.NoError(t, st.BindClientSessionDevice(
		t.Context(), "user-1", "session-1", testDeviceOne,
	))

	resp, envelope := deviceSettingsRequest(t, app, http.MethodGet,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, envelope)
	rows := envelope["devices"].([]any)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	assert.Empty(t, row["metadata"])
	assert.Empty(t, row["capabilities"])
	assert.IsType(t, map[string]any{}, row["metadata"])
	assert.IsType(t, []any{}, row["capabilities"])
}

func TestSettingsSelectedRejectsForeignDevice(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")
	seedClientDevice(t, st, "user-1", testDeviceOne, "Office PC")
	seedClientDevice(t, st, "other-user", testDeviceTwo, "Someone else's browser")
	require.NoError(t, st.BindClientSessionDevice(t.Context(), "user-1", "session-1", testDeviceOne))

	resp, _ := deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/privacy", testDeviceOne, "", map[string]any{
			"version": 1, "operation": "set",
			"target": map[string]any{"mode": "selected", "deviceIds": []string{testDeviceTwo}},
			"settings": map[string]any{
				"privacy": map[string]any{
					"storeAudio": false, "storeTranscripts": false, "retentionDays": 0,
				},
			},
		})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSettingsSelectedEnforcesCapabilities(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")
	seedClientDevice(t, st, "user-1", testDeviceOne, "Office PC")
	_, err := st.UpsertClientDevice(t.Context(), &store.Device{
		DeviceID: testDeviceTwo, UserID: "user-1", Name: "Privacy-only display",
		Surface: store.SurfaceWeb, Capabilities: []string{store.SettingsSectionPrivacy},
	})
	require.NoError(t, err)
	require.NoError(t, st.BindClientSessionDevice(t.Context(), "user-1", "session-1", testDeviceOne))

	resp, _ := deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", map[string]any{
			"version": 1, "operation": "set",
			"target": map[string]any{"mode": "selected", "deviceIds": []string{testDeviceTwo}},
			"settings": map[string]any{
				"wakeWord": "computer",
			},
		})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", map[string]any{
			"version": 1, "operation": "set",
			"target": map[string]any{"mode": "all", "deviceIds": []string{}},
			"settings": map[string]any{
				"wakeWord": "computer",
			},
		})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"apply-all must not claim success when an active host cannot use the section")
}

func TestSettingsPartialSelectedAndAllUseCorrectBases(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, "")
	interceptShadowPublish(t)
	seedClientDevice(t, st, "user-1", testDeviceOne, "Office PC")
	seedClientDevice(t, st, "user-1", testDeviceTwo, "Kitchen display")
	require.NoError(t, st.BindClientSessionDevice(t.Context(), "user-1", "session-1", testDeviceOne))

	doc := store.DefaultSettings()
	require.NoError(t, store.ApplySettingsSection(doc, store.SettingsSectionWakeWord,
		map[string]any{"wakeWord": "office", "wakeEngine": "porcupine", "sensitivity": 0.8},
		[]string{testDeviceOne}, false, false, time.Now()))
	require.NoError(t, store.ApplySettingsSection(doc, store.SettingsSectionWakeWord,
		map[string]any{"wakeWord": "kitchen", "wakeEngine": "wakenet", "sensitivity": 0.2},
		[]string{testDeviceTwo}, false, false, time.Now()))
	_, err := st.PutSettings(t.Context(), "user-1", doc, 1)
	require.NoError(t, err)

	resp, out := deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", map[string]any{
			"version": 2, "operation": "set",
			"target": map[string]any{"mode": "selected", "deviceIds": []string{testDeviceTwo}},
			"settings": map[string]any{
				"wakeWord": "updated-kitchen",
			},
		})
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	afterSelected, err := st.GetSettings(t.Context(), "user-1")
	require.NoError(t, err)
	kitchen := store.EffectiveSettings(afterSelected, testDeviceTwo)
	assert.Equal(t, "updated-kitchen", kitchen["wakeWord"])
	assert.Equal(t, "wakenet", kitchen["wakeEngine"])
	assert.Equal(t, 0.2, kitchen["sensitivity"])
	assert.Equal(t, "office", store.EffectiveSettings(afterSelected, testDeviceOne)["wakeWord"])

	resp, out = deviceSettingsRequest(t, app, http.MethodPatch,
		"/api/v1/settings/sections/wakeWord", testDeviceOne, "", map[string]any{
			"version": 3, "operation": "set",
			"target": map[string]any{"mode": "all", "deviceIds": []string{}},
			"settings": map[string]any{
				"wakeWord": "everyone",
			},
		})
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	afterAll, err := st.GetSettings(t.Context(), "user-1")
	require.NoError(t, err)
	assert.Equal(t, "everyone", afterAll["wakeWord"])
	assert.Equal(t, "openwakeword", afterAll["wakeEngine"],
		"partial all must preserve account defaults, not copy the current host")
	assert.Equal(t, 0.5, afterAll["sensitivity"])
}

func TestOwnedRequestDeviceAdjudicatesReboundSession(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, testDeviceOld)
	seedClientDevice(t, st, "user-1", testDeviceOld, "Old browser install")
	seedClientDevice(t, st, "user-1", testDeviceOne, "Recovered browser install")
	require.NoError(t, st.BindClientSessionDevice(t.Context(), "user-1", "session-1", testDeviceOne))

	doc := store.DefaultSettings()
	require.NoError(t, store.ApplySettingsSection(doc, store.SettingsSectionAppearance,
		map[string]any{"theme": "dark"}, []string{testDeviceOne}, false, false, time.Now()))
	_, err := st.PutSettings(t.Context(), "user-1", doc, 1)
	require.NoError(t, err)

	resp, effective := deviceSettingsRequest(t, app, http.MethodGet,
		"/api/v1/settings?effective=true", testDeviceOne, testDeviceOld, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, effective)
	assert.Equal(t, "dark", effective["theme"],
		"the session's new binding must bridge the stale access-JWT transition")
}

func TestTranscriptHonorsDevicePrivacy(t *testing.T) {
	app, deps, st := newDeviceSettingsApp(t, "")
	seedClientDevice(t, st, "user-1", testDeviceOne, "Private browser")
	require.NoError(t, st.BindClientSessionDevice(t.Context(), "user-1", "session-1", testDeviceOne))
	doc := store.DefaultSettings()
	require.NoError(t, store.ApplySettingsSection(doc, store.SettingsSectionPrivacy,
		map[string]any{"privacy": map[string]any{
			"storeAudio": false, "storeTranscripts": false, "retentionDays": 0,
		}}, []string{testDeviceOne}, false, false, time.Now()))
	_, err := st.PutSettings(t.Context(), "user-1", doc, 1)
	require.NoError(t, err)
	app.Post("/api/v1/transcript", handleTranscript(deps))

	resp, out := deviceSettingsRequest(t, app, http.MethodPost, "/api/v1/transcript",
		testDeviceOne, "", map[string]any{
			"sessionId": "conversation-1",
			"turns": []map[string]any{
				{"seq": 1, "role": "user", "text": "do not retain this", "engine": "openai-realtime"},
			},
		})
	require.Equal(t, http.StatusOK, resp.StatusCode, out)
	assert.Equal(t, float64(0), out["written"])
}

func TestEffectiveSettingsRejectsRevokedSignedDevice(t *testing.T) {
	app, _, st := newDeviceSettingsApp(t, testDeviceOne)
	seedClientDevice(t, st, "user-1", testDeviceOne, "Revoked browser")
	require.NoError(t, st.RevokeDevice(t.Context(), testDeviceOne))

	resp, _ := deviceSettingsRequest(t, app, http.MethodGet,
		"/api/v1/settings?effective=true", testDeviceOne, testDeviceOne, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"a matching signed did/header must not bypass the active device row")
}
