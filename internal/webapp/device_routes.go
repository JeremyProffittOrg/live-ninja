package webapp

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

const deviceIDHeaderName = "X-LN-Device-ID"

var deviceCapabilityRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// requestDeviceID resolves the temporary header-first identity used while a
// freshly registered web/Android session waits for its next access JWT. Every
// handler that consumes the result for authorization calls
// ownedRequestDeviceID, which verifies both claim consistency and ownership.
func requestDeviceID(c *fiber.Ctx) string {
	if id := strings.TrimSpace(c.Get(deviceIDHeaderName)); id != "" {
		return id
	}
	return strings.TrimSpace(DeviceID(c))
}

func deviceIdentityConflict(c *fiber.Ctx) bool {
	headerID := strings.TrimSpace(c.Get(deviceIDHeaderName))
	claimID := strings.TrimSpace(DeviceID(c))
	return headerID != "" && claimID != "" && headerID != claimID
}

// ownedRequestDeviceID verifies that the header/JWT device belongs to the
// authenticated user before any settings or runtime path accepts it. When
// required is false, a request with no device identity is allowed.
func ownedRequestDeviceID(c *fiber.Ctx, deps *Deps, required bool) (string, error) {
	headerID := strings.TrimSpace(c.Get(deviceIDHeaderName))
	claimID := strings.TrimSpace(DeviceID(c))
	if headerID == "" {
		if claimID != "" {
			// The did claim is signed and was already accepted by auth
			// middleware, but the device row is still checked below so a
			// revoked device cannot use an unexpired access token.
			headerID = claimID
		} else {
			if required {
				return "", apiBadRequest(c, "X-LN-Device-ID is required for this operation")
			}
			return "", nil
		}
	}
	if claimID != "" && claimID != headerID {
		// Storage-reset recovery can replace a web/Android session binding
		// while its already-issued access JWT still names the old install.
		// Adjudicate that narrow transition against the current session row.
		if Surface(c) != store.SurfaceWeb && Surface(c) != store.SurfaceAndroid {
			return "", apiBadRequest(c, "X-LN-Device-ID conflicts with the authenticated device")
		}
		session, err := deps.Store.GetSessionForUser(c.Context(), UserID(c), SessionID(c))
		if err != nil {
			return "", apiInternalError(c, deps, "get current session", err)
		}
		if session == nil || session.UserID != UserID(c) || session.DeviceID != headerID {
			return "", apiBadRequest(c, "X-LN-Device-ID conflicts with the authenticated device")
		}
	}
	deviceID := headerID
	if deviceID == "" {
		if required {
			return "", apiBadRequest(c, "X-LN-Device-ID is required for this operation")
		}
		return "", nil
	}
	if _, err := uuid.Parse(deviceID); err != nil {
		return "", apiBadRequest(c, "X-LN-Device-ID must be a UUID")
	}
	device, err := deps.Store.GetDevice(c.Context(), deviceID)
	if err != nil {
		return "", apiInternalError(c, deps, "get current device", err)
	}
	if device == nil || device.UserID != UserID(c) || device.Status != store.DeviceStatusActive {
		return "", errorJSON(c, fiber.StatusNotFound, "not_found", "Device not found.")
	}
	return deviceID, nil
}

func deviceResponse(device store.Device, current bool) fiber.Map {
	lastSeen := ""
	lastSeenAt := device.LastSeenAt
	if lastSeenAt == 0 {
		lastSeenAt = device.CreatedAt
	}
	if lastSeenAt > 0 {
		lastSeen = time.Unix(lastSeenAt, 0).UTC().Format(time.RFC3339)
	}
	updated := ""
	if device.UpdatedAt > 0 {
		updated = time.Unix(device.UpdatedAt, 0).UTC().Format(time.RFC3339)
	}
	metadata := device.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	capabilities := device.Capabilities
	if capabilities == nil {
		capabilities = []string{}
	}
	surface := device.Surface
	if surface == "" {
		surface = store.SurfaceDevice
	}
	return fiber.Map{
		"deviceId":     device.DeviceID,
		"name":         device.Name,
		"status":       device.Status,
		"surface":      surface,
		"metadata":     metadata,
		"capabilities": capabilities,
		"thingName":    device.ThingName,
		"createdAt":    device.CreatedAt,
		"updatedAt":    updated,
		"lastSeenAt":   lastSeen,
		"isCurrent":    current,
	}
}

func includeCurrentDevice(c *fiber.Ctx, deps *Deps, devices []store.Device, currentDeviceID string) ([]store.Device, error) {
	if currentDeviceID == "" {
		return devices, nil
	}
	for _, device := range devices {
		if device.DeviceID == currentDeviceID {
			return devices, nil
		}
	}
	device, err := deps.Store.GetDevice(c.Context(), currentDeviceID)
	if err != nil {
		return nil, apiInternalError(c, deps, "get current device fallback", err)
	}
	if device == nil || device.UserID != UserID(c) || device.Status != store.DeviceStatusActive {
		return nil, errorJSON(c, fiber.StatusNotFound, "not_found", "Device not found.")
	}
	return append([]store.Device{*device}, devices...), nil
}

func handleRegisterCurrentDevice(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var body struct {
			DeviceID      string         `json:"deviceId"`
			AppInstanceID string         `json:"appInstanceId"`
			SuggestedName string         `json:"suggestedName"`
			Metadata      map[string]any `json:"metadata"`
			Capabilities  []string       `json:"capabilities"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		headerID := strings.TrimSpace(c.Get(deviceIDHeaderName))
		bodyID := strings.TrimSpace(body.DeviceID)
		if bodyID == "" {
			bodyID = strings.TrimSpace(body.AppInstanceID)
		}
		if headerID != "" && bodyID != "" && headerID != bodyID {
			return apiBadRequest(c, "deviceId must match X-LN-Device-ID")
		}
		deviceID := headerID
		if deviceID == "" {
			deviceID = bodyID
		}
		parsedID, err := uuid.Parse(deviceID)
		if err != nil {
			return apiBadRequest(c, "X-LN-Device-ID must be a UUID")
		}
		deviceID = parsedID.String()

		name, err := normalizeDeviceName(body.SuggestedName)
		if err != nil {
			return apiBadRequest(c, err.Error())
		}
		metadata, err := normalizeDeviceMetadata(body.Metadata)
		if err != nil {
			return apiBadRequest(c, err.Error())
		}
		capabilities, err := normalizeDeviceCapabilities(body.Capabilities)
		if err != nil {
			return apiBadRequest(c, err.Error())
		}
		surface := Surface(c)
		if surface != store.SurfaceWeb && surface != store.SurfaceAndroid {
			return apiBadRequest(c, "authenticated device surface is invalid")
		}
		if name == "" {
			name = inferredDeviceName(surface, metadata)
		}
		// Inferred names concatenate client metadata, so validate the final
		// display value too rather than validating only suggestedName.
		name, err = normalizeDeviceName(name)
		if err != nil || name == "" {
			if err != nil {
				return apiBadRequest(c, err.Error())
			}
			return apiBadRequest(c, "device name is required")
		}

		// Reject an already-bound session before creating a new directory
		// entry. BindSessionDevice repeats this check conditionally after the
		// upsert so a concurrent registration cannot switch the binding.
		sessionID := SessionID(c)
		if sessionID == "" {
			return apiBadRequest(c, "authenticated session is required")
		}
		session, err := deps.Store.GetSessionForUser(c.Context(), UserID(c), sessionID)
		if err != nil {
			return apiInternalError(c, deps, "get current session", err)
		}
		if session == nil || session.UserID != UserID(c) {
			return errorJSON(c, fiber.StatusUnauthorized, "unauthorized", "Session is no longer active.")
		}
		if session.DeviceID != "" && session.DeviceID != deviceID {
			if session.Surface != store.SurfaceWeb && session.Surface != store.SurfaceAndroid {
				return errorJSON(c, fiber.StatusConflict, "device_conflict",
					"This session is already bound to another device.")
			}
			// Reject a stale revoked-bound session before UpsertClientDevice
			// can leave an active orphan for each rotated retry. The bind
			// transaction repeats this status condition to close a revoke
			// racing after this preflight.
			oldDevice, getErr := deps.Store.GetDevice(c.Context(), session.DeviceID)
			if getErr != nil {
				return apiInternalError(c, deps, "get bound device before registration", getErr)
			}
			if oldDevice == nil || oldDevice.UserID != UserID(c) {
				return errorJSON(c, fiber.StatusConflict, "device_conflict",
					"This session is already bound to another device.")
			}
			if oldDevice.Status != store.DeviceStatusActive {
				return errorJSON(c, fiber.StatusConflict, "device_revoked",
					"This session belongs to a revoked device. Sign in again before registering a new device identity.")
			}
		}
		if deviceIdentityConflict(c) &&
			surface != store.SurfaceWeb && surface != store.SurfaceAndroid {
			return apiBadRequest(c, "X-LN-Device-ID conflicts with the authenticated device")
		}

		device, err := deps.Store.UpsertClientDevice(c.Context(), &store.Device{
			DeviceID:     deviceID,
			UserID:       UserID(c),
			Name:         name,
			Surface:      surface,
			Metadata:     metadata,
			Capabilities: capabilities,
			FamilyID:     session.FamilyID,
		})
		if err != nil {
			switch {
			case errors.Is(err, store.ErrDeviceOwnership):
				return errorJSON(c, fiber.StatusConflict, "device_conflict", "That device identity cannot be registered.")
			case errors.Is(err, store.ErrDeviceRevoked):
				return errorJSON(c, fiber.StatusConflict, "device_revoked", "This device was revoked. Sign in again to create a new device identity.")
			case errors.Is(err, store.ErrDeviceBindingConflict):
				return errorJSON(c, fiber.StatusConflict, "device_conflict", "That device identity is registered to a different device type.")
			default:
				return apiInternalError(c, deps, "register current device", err)
			}
		}
		if err := deps.Store.BindClientSessionDevice(c.Context(), UserID(c), sessionID, deviceID); err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				return errorJSON(c, fiber.StatusUnauthorized, "unauthorized", "Session is no longer active.")
			case errors.Is(err, store.ErrDeviceRevoked):
				return errorJSON(c, fiber.StatusConflict, "device_revoked",
					"This session belongs to a revoked device. Sign in again before registering a new device identity.")
			case errors.Is(err, store.ErrDeviceBindingConflict):
				return errorJSON(c, fiber.StatusConflict, "device_conflict", "This session is already bound to another device.")
			default:
				return apiInternalError(c, deps, "bind current device", err)
			}
		}
		if session.DeviceID != "" && session.DeviceID != deviceID && session.FamilyID != "" {
			if err := deps.Store.DetachDeviceFamily(
				c.Context(), UserID(c), session.DeviceID, session.FamilyID,
			); err != nil && !errors.Is(err, store.ErrDeviceBindingConflict) {
				deps.Log.Warn("api: detach stale device family failed",
					"deviceId", session.DeviceID, "error", err.Error())
			}
		}
		return c.JSON(fiber.Map{"device": deviceResponse(*device, true)})
	}
}

func handleRenameDevice(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		deviceID := strings.TrimSpace(c.Params("id"))
		if _, err := uuid.Parse(deviceID); err != nil {
			return apiBadRequest(c, "device id must be a UUID")
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		name, err := normalizeDeviceName(body.Name)
		if err != nil || name == "" {
			if err != nil {
				return apiBadRequest(c, err.Error())
			}
			return apiBadRequest(c, "name is required")
		}
		device, err := deps.Store.RenameDevice(c.Context(), UserID(c), deviceID, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errorJSON(c, fiber.StatusNotFound, "not_found", "Device not found.")
			}
			return apiInternalError(c, deps, "rename device", err)
		}
		return c.JSON(fiber.Map{"device": deviceResponse(*device, deviceID == requestDeviceID(c))})
	}
}

func normalizeDeviceName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if len([]rune(name)) > 80 {
		return "", errors.New("device name must be at most 80 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("device name cannot contain control characters")
		}
	}
	return name, nil
}

func normalizeDeviceMetadata(raw map[string]any) (map[string]string, error) {
	if len(raw) > 16 {
		return nil, errors.New("metadata may contain at most 16 fields")
	}
	allowed := map[string]bool{
		"surface": true, "browser": true, "platform": true, "deviceClass": true,
		"manufacturer": true, "model": true, "product": true, "androidSdk": true,
		"appVersion": true, "osVersion": true, "clientName": true,
		"clientVersion": true, "firmwareVersion": true,
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if !allowed[key] {
			continue // discard fingerprint-like or future unsafe attributes
		}
		var display string
		switch typed := value.(type) {
		case string:
			display = strings.TrimSpace(typed)
		case float64:
			display = strconv.FormatFloat(typed, 'f', -1, 64)
		case nil:
			continue
		default:
			return nil, fmt.Errorf("metadata.%s must be a string or number", key)
		}
		if len([]rune(display)) > 128 {
			return nil, fmt.Errorf("metadata.%s must be at most 128 characters", key)
		}
		for _, r := range display {
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("metadata.%s cannot contain control characters", key)
			}
		}
		if display != "" {
			out[key] = display
		}
	}
	return out, nil
}

func normalizeDeviceCapabilities(raw []string) ([]string, error) {
	if len(raw) > 32 {
		return nil, errors.New("capabilities may contain at most 32 values")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, rawCapability := range raw {
		capability := strings.TrimSpace(rawCapability)
		if !deviceCapabilityRE.MatchString(capability) {
			return nil, errors.New("capabilities must use letters, numbers, dot, underscore, colon, or dash")
		}
		if !seen[capability] {
			seen[capability] = true
			out = append(out, capability)
		}
	}
	return out, nil
}

func inferredDeviceName(surface string, metadata map[string]string) string {
	switch surface {
	case store.SurfaceAndroid:
		if model := strings.TrimSpace(metadata["model"]); model != "" {
			if manufacturer := strings.TrimSpace(metadata["manufacturer"]); manufacturer != "" &&
				!strings.EqualFold(manufacturer, model) {
				return manufacturer + " " + model
			}
			return model
		}
		return "Android device"
	case store.SurfaceWeb:
		browser := strings.TrimSpace(metadata["browser"])
		platform := strings.TrimSpace(metadata["platform"])
		if browser != "" && platform != "" {
			return browser + " on " + platform
		}
		return "Web browser"
	default:
		return "Live Ninja Device"
	}
}
