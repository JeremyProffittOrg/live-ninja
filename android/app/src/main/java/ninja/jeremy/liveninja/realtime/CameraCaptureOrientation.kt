package ninja.jeremy.liveninja.realtime

/**
 * Computes the clockwise rotation written to captured-media metadata.
 *
 * Both inputs are expressed relative to the device's natural orientation.
 * Front-facing sensors use the opposite display-rotation sign from back-facing
 * sensors because their image coordinate system is mirrored.
 */
internal object CameraCaptureOrientation {
    private val validOrientations = setOf(0, 90, 180, 270)

    fun outputRotationDegrees(
        sensorOrientationDegrees: Int,
        displayRotationDegrees: Int,
        lens: CameraLens,
    ): Int {
        require(sensorOrientationDegrees in validOrientations) {
            "sensor orientation must be a multiple of 90 degrees"
        }
        require(displayRotationDegrees in validOrientations) {
            "display rotation must be a multiple of 90 degrees"
        }

        val lensAdjustedDisplayRotation = when (lens) {
            CameraLens.BACK -> -displayRotationDegrees
            CameraLens.FRONT -> displayRotationDegrees
        }
        return Math.floorMod(sensorOrientationDegrees + lensAdjustedDisplayRotation, 360)
    }
}
