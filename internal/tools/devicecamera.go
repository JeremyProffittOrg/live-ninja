package tools

// Device-local Android camera capture (plan.md M28). The voice command is the
// user's confirmation: the client captures immediately after a valid tool call
// and must not add a second confirmation dialog. Camera permission itself is
// still an explicit Android runtime grant handled during onboarding/Settings.
var deviceCameraLenses = []string{"back", "front"}

const maxVideoDurationSeconds = 300

func takePhotoDefinition() *Definition {
	return &Definition{
		Name: "take_photo",
		Description: "Take a JPEG photo now with the camera on the user's current device, " +
			"upload it to their private Files storage, and return the saved file. The spoken " +
			"request is the capture confirmation: do not ask for an additional confirmation. " +
			"Omit camera unless the user explicitly requests the front camera; back is the default.",
		Params: []ParamSpec{
			{
				Name: "camera",
				Type: "string",
				Description: "Camera lens to use. Omit when unspecified; back is the default. " +
					"Use front only when the user explicitly requests it.",
				Enum: deviceCameraLenses,
			},
		},
		DeviceLocal: true,
		Surfaces:    []string{"android"},
		Handler:     handleDeviceLocalOnly,
	}
}

func recordVideoDefinition() *Definition {
	return &Definition{
		Name: "record_video",
		Description: "Record a silent MP4 video now with the camera on the user's current device " +
			"(the live voice session keeps the microphone), " +
			"upload it to their private Files storage, and return the saved file. The spoken " +
			"request is the capture confirmation: do not ask for an additional confirmation. " +
			"Omit camera unless the user explicitly requests the front camera; back is the default. " +
			"Omit durationSeconds unless the user states a duration; the default is 60 seconds.",
		Params: []ParamSpec{
			{
				Name: "camera",
				Type: "string",
				Description: "Camera lens to use. Omit when unspecified; back is the default. " +
					"Use front only when the user explicitly requests it.",
				Enum: deviceCameraLenses,
			},
			{
				Name: "durationSeconds",
				Type: "integer",
				Description: "Recording duration in whole seconds. Omit when unspecified to " +
					"default to 60 seconds; maximum 300 seconds.",
				Min: floatPtr(1),
				Max: floatPtr(maxVideoDurationSeconds),
			},
		},
		DeviceLocal: true,
		Surfaces:    []string{"android"},
		Handler:     handleDeviceLocalOnly,
	}
}
