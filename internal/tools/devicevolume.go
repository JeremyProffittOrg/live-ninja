package tools

// Device-local Android volume control (plan.md M27). These are the public
// AudioManager streams whose volume index Android lets an application address
// directly. "media" intentionally comes first because it is the owner-selected
// default when the user does not name a stream.
var deviceVolumeStreams = []string{
	"media",
	"ring",
	"notification",
	"alarm",
	"system",
	"voice_call",
	"dtmf",
	"accessibility",
}

var deviceVolumeActions = []string{
	"set",
	"increase",
	"decrease",
	"mute",
	"unmute",
}

func setVolumeDefinition() *Definition {
	return &Definition{
		Name: "set_volume",
		Description: "Set or adjust an audio-stream volume on the user's current device. " +
			"Use action=set with level (0-100) for an absolute volume; use increase or " +
			"decrease for one device volume step, or mute/unmute as requested. If the user " +
			"does not name a stream, omit stream so media is used. Only target ring, " +
			"notification, alarm, system, voice_call, dtmf, or accessibility when the user " +
			"explicitly names that kind of audio.",
		Params: []ParamSpec{
			{
				Name:        "action",
				Type:        "string",
				Description: "How to change the volume.",
				Required:    true,
				Enum:        deviceVolumeActions,
			},
			{
				Name: "level",
				Type: "integer",
				Description: "Target volume percentage from 0 to 100. Include this only " +
					"when action is set.",
				Min: floatPtr(0),
				Max: floatPtr(100),
			},
			{
				Name: "stream",
				Type: "string",
				Description: "Audio stream to change. Omit this when the user did not " +
					"name one; media is the default.",
				Enum: deviceVolumeStreams,
			},
		},
		DeviceLocal: true,
		Handler:     handleDeviceLocalOnly,
	}
}
