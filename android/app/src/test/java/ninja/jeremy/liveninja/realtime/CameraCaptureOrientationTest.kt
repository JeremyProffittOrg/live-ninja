package ninja.jeremy.liveninja.realtime

import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Test

class CameraCaptureOrientationTest {
    @Test
    fun `sensor 90 covers every display rotation and lens`() {
        val expected = mapOf(
            CameraLens.BACK to mapOf(
                0 to 90,
                90 to 0,
                180 to 270,
                270 to 180,
            ),
            CameraLens.FRONT to mapOf(
                0 to 90,
                90 to 180,
                180 to 270,
                270 to 0,
            ),
        )

        expected.forEach { (lens, rotations) ->
            rotations.forEach { (displayRotation, outputRotation) ->
                assertEquals(
                    "$lens lens at display rotation $displayRotation",
                    outputRotation,
                    CameraCaptureOrientation.outputRotationDegrees(
                        sensorOrientationDegrees = 90,
                        displayRotationDegrees = displayRotation,
                        lens = lens,
                    ),
                )
            }
        }
    }

    @Test
    fun `sensor 270 covers every display rotation and lens`() {
        val expected = mapOf(
            CameraLens.BACK to mapOf(
                0 to 270,
                90 to 180,
                180 to 90,
                270 to 0,
            ),
            CameraLens.FRONT to mapOf(
                0 to 270,
                90 to 0,
                180 to 90,
                270 to 180,
            ),
        )

        expected.forEach { (lens, rotations) ->
            rotations.forEach { (displayRotation, outputRotation) ->
                assertEquals(
                    "$lens lens at display rotation $displayRotation",
                    outputRotation,
                    CameraCaptureOrientation.outputRotationDegrees(
                        sensorOrientationDegrees = 270,
                        displayRotationDegrees = displayRotation,
                        lens = lens,
                    ),
                )
            }
        }
    }

    @Test
    fun `rejects non-right-angle inputs`() {
        assertThrows(IllegalArgumentException::class.java) {
            CameraCaptureOrientation.outputRotationDegrees(
                sensorOrientationDegrees = 45,
                displayRotationDegrees = 0,
                lens = CameraLens.BACK,
            )
        }
        assertThrows(IllegalArgumentException::class.java) {
            CameraCaptureOrientation.outputRotationDegrees(
                sensorOrientationDegrees = 90,
                displayRotationDegrees = 45,
                lens = CameraLens.FRONT,
            )
        }
    }
}
