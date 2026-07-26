package webapp

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	lnsync "github.com/JeremyProffittOrg/live-ninja/internal/sync"
)

type settingsSectionTarget struct {
	Mode      string   `json:"mode"`
	DeviceIDs []string `json:"deviceIds"`
}

type settingsSectionPatch struct {
	Version   int64                 `json:"version"`
	Operation string                `json:"operation"`
	Target    settingsSectionTarget `json:"target"`
	Settings  map[string]any        `json:"settings"`
}

func handleGetSettingsSection(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		section := c.Params("section")
		if _, ok := store.SettingsSectionFields(section); !ok {
			return apiBadRequest(c, "unknown settings section")
		}
		doc, err := deps.Store.GetSettings(c.Context(), UserID(c))
		if err != nil {
			return apiInternalError(c, deps, "get settings section", err)
		}
		devices, err := deps.Store.ListDevices(c.Context(), UserID(c))
		if err != nil {
			return apiInternalError(c, deps, "list settings devices", err)
		}
		currentDeviceID, responseErr := ownedRequestDeviceID(c, deps, false)
		if responseErr != nil {
			return responseErr
		}
		devices, responseErr = includeCurrentDevice(c, deps, devices, currentDeviceID)
		if responseErr != nil {
			return responseErr
		}
		return c.JSON(settingsSectionEnvelope(doc, devices, currentDeviceID, section))
	}
}

func handlePatchSettingsSection(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := UserID(c)
		section := c.Params("section")
		if _, ok := store.SettingsSectionFields(section); !ok {
			return apiBadRequest(c, "unknown settings section")
		}

		var body settingsSectionPatch
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if body.Version < 1 {
			return apiBadRequest(c, "version must be a positive integer (the version you last read)")
		}
		if body.Operation != "set" && body.Operation != "inherit" {
			return apiBadRequest(c, "operation must be set or inherit")
		}
		if body.Operation == "set" && len(body.Settings) == 0 {
			return apiBadRequest(c, "settings object is required for operation set")
		}
		if body.Operation == "inherit" && len(body.Settings) != 0 {
			return apiBadRequest(c, "settings must be empty for operation inherit")
		}

		doc, err := deps.Store.GetSettings(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "get settings section", err)
		}
		devices, err := deps.Store.ListDevices(c.Context(), userID)
		if err != nil {
			return apiInternalError(c, deps, "list settings devices", err)
		}
		currentDeviceID, responseErr := ownedRequestDeviceID(c, deps, false)
		if responseErr != nil {
			return responseErr
		}
		devices, responseErr = includeCurrentDevice(c, deps, devices, currentDeviceID)
		if responseErr != nil {
			return responseErr
		}
		owned := make(map[string]store.Device, len(devices))
		for _, device := range devices {
			if device.Status == store.DeviceStatusActive {
				owned[device.DeviceID] = device
			}
		}
		targetIDs, all, validationErr := resolveSettingsTargets(body, currentDeviceID, owned, section)
		if validationErr != "" {
			return apiBadRequest(c, validationErr)
		}

		payloads := map[string]map[string]any{}
		var allPayload map[string]any
		if body.Operation == "set" {
			if section == store.SettingsSectionMicrophone &&
				(all || body.Target.Mode == "selected") {
				if mic, present := body.Settings["micDeviceId"]; present && mic != nil && mic != "" {
					if all {
						return apiBadRequest(c, "a non-default microphone cannot be applied to all devices")
					}
					return apiBadRequest(c, "a non-default microphone can only be saved for the current device")
				}
			}
			if all {
				// A partial apply-all is based on account defaults, never the
				// calling host's override.
				allPayload, validationErr = normalizedSettingsSectionPayload(doc, body.Settings, section)
				if validationErr != "" {
					return apiBadRequest(c, validationErr)
				}
			} else {
				// Partial selected writes preserve every target's unmentioned
				// effective values rather than copying the current host's.
				for _, deviceID := range targetIDs {
					base := store.EffectiveSettings(doc, deviceID)
					payloads[deviceID], validationErr = normalizedSettingsSectionPayload(base, body.Settings, section)
					if validationErr != "" {
						return apiBadRequest(c, validationErr)
					}
				}
			}
		}

		now := time.Now().UTC()
		if all || body.Operation == "inherit" {
			if err := store.ApplySettingsSection(
				doc, section, allPayload, targetIDs, all,
				body.Operation == "inherit", now,
			); err != nil {
				return apiBadRequest(c, err.Error())
			}
		} else {
			for _, deviceID := range targetIDs {
				if err := store.ApplySettingsSection(
					doc, section, payloads[deviceID], []string{deviceID}, false, false, now,
				); err != nil {
					return apiBadRequest(c, err.Error())
				}
			}
		}
		newVersion, err := deps.Store.PutSettings(c.Context(), userID, doc, body.Version)
		if err != nil {
			if errors.Is(err, store.ErrVersionConflict) {
				return errorJSON(c, fiber.StatusConflict, "version_conflict",
					"Your settings were changed from another device. Re-read and re-apply.")
			}
			if errors.Is(err, store.ErrSettingsTooLarge) {
				return errorJSON(c, fiber.StatusRequestEntityTooLarge, "settings_too_large",
					"Settings are too large to save. Remove some device-specific or custom data.")
			}
			return apiInternalError(c, deps, "put settings section", err)
		}
		doc["version"] = newVersion
		publishSettingsShadow(c.Context(), deps, userID, doc, newVersion)
		return c.JSON(settingsSectionEnvelope(doc, devices, currentDeviceID, section))
	}
}

func normalizedSettingsSectionPayload(base, patch map[string]any, section string) (map[string]any, string) {
	candidate, err := store.MergeSettingsSection(base, patch, section)
	if err != nil {
		return nil, err.Error()
	}
	if msg := validateAndNormalizeSettings(candidate); msg != "" {
		return nil, msg
	}
	payload, _ := store.ExtractSettingsSection(candidate, section)
	return payload, ""
}

func resolveSettingsTargets(body settingsSectionPatch, currentDeviceID string,
	owned map[string]store.Device, section string) ([]string, bool, string) {
	switch body.Target.Mode {
	case "current":
		if len(body.Target.DeviceIDs) != 0 {
			return nil, false, "target.deviceIds must be empty for mode current"
		}
		if currentDeviceID == "" {
			return nil, false, "X-LN-Device-ID is required for mode current"
		}
		if device, ok := owned[currentDeviceID]; !ok || !deviceSupportsSection(device, section) {
			return nil, false, "the current device does not support this settings section"
		}
		return []string{currentDeviceID}, false, ""
	case "selected":
		if len(body.Target.DeviceIDs) == 0 {
			return nil, false, "target.deviceIds is required for mode selected"
		}
		seen := map[string]bool{}
		targetIDs := make([]string, 0, len(body.Target.DeviceIDs))
		for _, rawID := range body.Target.DeviceIDs {
			deviceID := strings.TrimSpace(rawID)
			if _, err := uuid.Parse(deviceID); err != nil {
				return nil, false, "target.deviceIds must contain UUIDs"
			}
			device, ok := owned[deviceID]
			if !ok {
				return nil, false, "target.deviceIds contains a device not owned by this account"
			}
			if !deviceSupportsSection(device, section) {
				return nil, false, "target.deviceIds contains a device that does not support this settings section"
			}
			if !seen[deviceID] {
				seen[deviceID] = true
				targetIDs = append(targetIDs, deviceID)
			}
		}
		return targetIDs, false, ""
	case "all":
		if body.Operation == "inherit" {
			return nil, false, "operation inherit cannot use mode all"
		}
		if len(body.Target.DeviceIDs) != 0 {
			return nil, false, "target.deviceIds must be empty for mode all"
		}
		for _, device := range owned {
			if !deviceSupportsSection(device, section) {
				return nil, false, "not every active device supports this settings section"
			}
		}
		return nil, true, ""
	default:
		return nil, false, "target.mode must be current, selected, or all"
	}
}

func deviceSupportsSection(device store.Device, section string) bool {
	if len(device.Capabilities) == 0 {
		return true // paired/legacy devices predate capability declarations
	}
	for _, capability := range device.Capabilities {
		if capability == section {
			return true
		}
	}
	return false
}

func settingsSectionEnvelope(doc map[string]any, devices []store.Device,
	currentDeviceID, section string) fiber.Map {
	accountDefaults, _ := store.ExtractSettingsSection(doc, section)
	rows := make([]fiber.Map, 0, len(devices))
	for _, device := range devices {
		if device.Status != store.DeviceStatusActive {
			continue
		}
		// Legacy device rows predate metadata/capability declarations. Emit
		// empty JSON collections rather than null so strict mobile decoders can
		// adopt the section envelope without falling back to local settings.
		metadata := device.Metadata
		if metadata == nil {
			metadata = map[string]string{}
		}
		capabilities := device.Capabilities
		if capabilities == nil {
			capabilities = []string{}
		}
		effective := store.EffectiveSettings(doc, device.DeviceID)
		settings, _ := store.ExtractSettingsSection(effective, section)
		surface := device.Surface
		if surface == "" {
			surface = store.SurfaceDevice
		}
		rows = append(rows, fiber.Map{
			"deviceId":     device.DeviceID,
			"name":         device.Name,
			"surface":      surface,
			"metadata":     metadata,
			"capabilities": capabilities,
			"isCurrent":    device.DeviceID == currentDeviceID,
			"inherited":    store.DeviceSectionInherited(doc, device.DeviceID, section),
			"settings":     settings,
		})
	}
	return fiber.Map{
		"section":         section,
		"version":         lnsync.DocVersion(doc),
		"currentDeviceId": nullableDeviceID(currentDeviceID),
		"accountDefaults": accountDefaults,
		"devices":         rows,
	}
}

func nullableDeviceID(deviceID string) any {
	if deviceID == "" {
		return nil
	}
	return deviceID
}
