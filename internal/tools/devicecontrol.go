package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
)

// device_control publishes an action to a device's MQTT downlink topic
// liveninja/<thingName>/control/down. It is strictly owner-scoped: the
// DEVICE#<deviceId>/META item must belong to the calling user and be
// active — a caller can never drive another user's hardware, and the
// deviceId argument is validated against the store, not trusted.
//
// The action list is a fixed enum shared with the M5Stack firmware's
// control handler; growing it means updating both ends deliberately.
var deviceControlActions = []string{
	"ping",
	"identify",
	"reboot",
	"mute",
	"unmute",
	"volume_up",
	"volume_down",
	"screen_on",
	"screen_off",
}

func deviceControlDefinition() *Definition {
	return &Definition{
		Name: "device_control",
		Description: "Control one of the user's own Live Ninja devices (e.g. the M5Stack): " +
			"ping/identify it, reboot it, mute/unmute, adjust volume, or turn the screen on/off.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "deviceId", Type: "string", Required: true, MinLen: 1, MaxLen: 128,
				Description: "The target device's id (from the user's registered device list)."},
			{Name: "action", Type: "string", Required: true, Enum: deviceControlActions,
				Description: "The control action to send."},
		},
		Handler: handleDeviceControl,
	}
}

func handleDeviceControl(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
	if deps.IoT == nil {
		return nil, toolErrf(CodeNotConfigured, "device control is not configured")
	}

	deviceID := args["deviceId"].(string)
	action := args["action"].(string)

	item, err := deps.Store.GetItem(ctx, "DEVICE#"+deviceID, "META")
	if err != nil {
		deps.Log.Error("tools: device lookup failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "device lookup failed")
	}
	if item == nil {
		return nil, toolErrf(CodeNotFound, "no device with id %q", deviceID)
	}

	// Ownership + status checks against the DEVICE item shape
	// (deviceId/userId/thingName/status — shared spec).
	ownerID, _ := item["userId"].(string)
	if ownerID == "" || ownerID != inv.UserID {
		// Deliberately the same shape as not_found so a probing caller
		// can't enumerate other users' device ids.
		return nil, toolErrf(CodeNotFound, "no device with id %q", deviceID)
	}
	if status, _ := item["status"].(string); status != "active" {
		return nil, toolErrf(CodeForbidden, "device %q is not active", deviceID)
	}
	thingName, _ := item["thingName"].(string)
	if thingName == "" {
		return nil, toolErrf(CodeNotFound, "device %q has no IoT thing bound yet (pair it first)", deviceID)
	}

	payload, _ := json.Marshal(map[string]any{
		"action": action,
		"ts":     deps.Now().UTC().Format(time.RFC3339),
	})
	topic := fmt.Sprintf("liveninja/%s/control/down", thingName)
	if _, err := deps.IoT.Publish(ctx, &iotdataplane.PublishInput{
		Topic:   aws.String(topic),
		Qos:     1,
		Payload: payload,
	}); err != nil {
		deps.Log.Error("tools: iot publish failed", "error", err.Error(), "topic", topic)
		return nil, toolErrf(CodeUpstreamError, "failed to reach the device")
	}

	return map[string]any{
		"status":   "sent",
		"deviceId": deviceID,
		"action":   action,
	}, nil
}
