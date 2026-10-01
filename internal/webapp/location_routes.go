package webapp

import (
	"fmt"
	"math"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// POST /api/v1/location/current — a device reports its own GPS fix (the
// Android app does this at the start of a live session when the user granted
// location). It is the same write the set_current_location tool makes when the
// user reads coordinates aloud, so it runs through the tool router: one
// validation path, one timezone lookup, one audit line.
//
// A fix within currentLocationSameSpotKm of the stored GPS location is a
// no-op, so a phone that sits still does not bump the settings version (and
// fan a sync out to every surface) on every session. A fix within the same
// distance of HOME is a no-op when no current location is set, and clears
// the current location when one is (the phone is back home).

const currentLocationSameSpotKm = 2.0

// currentLocationRetryWindow is the idempotency bucket for a device's fix
// reports: a client retry of the same POST inside it is a duplicate.
const currentLocationRetryWindow = 5 * time.Minute

func handleReportCurrentLocation(deps *Deps, registry *tools.Registry) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, sessionID, surface := UserID(c), SessionID(c), Surface(c)
		deviceID, responseErr := ownedRequestDeviceID(c, deps, false)
		if responseErr != nil {
			return responseErr
		}
		if registry == nil {
			return errorJSON(c, fiber.StatusServiceUnavailable, "not_configured", "the tool router is not configured")
		}

		var body struct {
			Latitude       *float64 `json:"latitude"`
			Longitude      *float64 `json:"longitude"`
			AccuracyMeters float64  `json:"accuracyMeters"`
		}
		if err := c.BodyParser(&body); err != nil {
			return apiBadRequest(c, "invalid JSON body")
		}
		if body.Latitude == nil || body.Longitude == nil {
			return apiBadRequest(c, "latitude and longitude are required")
		}
		lat, lon := *body.Latitude, *body.Longitude
		// Range-check before the dedupe below: an out-of-range fix must be a
		// 400, never an "unchanged" 200 because it happens to sit near the
		// stored point. (JSON cannot carry NaN or Inf.)
		if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			return apiBadRequest(c, "latitude must be within -90..90 and longitude within -180..180")
		}

		args := map[string]any{"latitude": lat, "longitude": lon}
		keyPart := fmt.Sprintf("%.3f,%.3f", lat, lon)
		if deps.Store != nil {
			if p, err := deps.Store.GetProfile(c.Context(), userID); err == nil {
				cur := p.Current()
				home := p.Home()
				atHome := home.Resolved() &&
					haversineKm(home.Lat, home.Lon, lat, lon) < currentLocationSameSpotKm
				switch {
				case atHome && !cur.Resolved():
					// The phone is at home and home already applies: storing
					// the fix as a "current location" would undo "I'm back
					// home" on the very next session.
					return c.JSON(fiber.Map{"status": "unchanged"})
				case atHome && p.CurrentLocationSource != store.CurrentLocationSourceGPS:
					// A spoken "I'm in Denver" outranks a device that is
					// sitting at home (a tablet left behind on a trip):
					// only the user's own "I'm back home" clears it.
					return c.JSON(fiber.Map{"status": "unchanged"})
				case atHome:
					// A GPS-set trip location and the device is back home:
					// clear it so home applies again.
					args = map[string]any{"home": true}
					keyPart = "home"
				case cur.Resolved() && p.CurrentLocationSource == store.CurrentLocationSourceGPS &&
					haversineKm(cur.Lat, cur.Lon, lat, lon) < currentLocationSameSpotKm:
					return c.JSON(fiber.Map{"status": "unchanged"})
				}
			}
		}

		res := registry.Invoke(c.Context(), tools.Invocation{
			Tool: "set_current_location",
			Args: args,
			// The key is the rounded fix plus a short time bucket, so a
			// retried POST of the same report is a duplicate, while a later
			// return to the same spot (A -> B -> A within the 24 h marker
			// TTL) is a real write instead of a silently dropped one.
			IdempotencyKey: fmt.Sprintf("devloc#%s#%s#%d", deviceID, keyPart,
				time.Now().Unix()/int64(currentLocationRetryWindow/time.Second)),
			TxID:      TxID(c),
			UserID:    userID,
			SessionID: sessionID,
			Surface:   surface,
			DeviceID:  deviceID,
			Role:      Role(c),
		})
		return c.Status(res.StatusCode()).JSON(res)
	}
}

// haversineKm is the great-circle distance between two coordinates.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusKm = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Sqrt(a))
}
