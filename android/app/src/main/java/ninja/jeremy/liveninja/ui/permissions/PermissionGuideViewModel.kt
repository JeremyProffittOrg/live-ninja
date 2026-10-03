package ninja.jeremy.liveninja.ui.permissions

import androidx.lifecycle.ViewModel
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import ninja.jeremy.liveninja.ui.state.ConsentEvent
import ninja.jeremy.liveninja.ui.state.ConsentLog

/** Keep the existing microphone/camera disclosure and decision audit when using the guide. */
@HiltViewModel
class PermissionGuideViewModel @Inject constructor(private val consentLog: ConsentLog) : ViewModel() {
    fun onDisclosure(feature: PermissionFeature) {
        val event = when (feature) {
            PermissionFeature.MICROPHONE -> ConsentEvent.MIC_DISCLOSURE_SHOWN
            PermissionFeature.CAMERA -> ConsentEvent.CAMERA_DISCLOSURE_SHOWN
            else -> return
        }
        consentLog.record(event, "permission_guide")
    }

    fun onResult(feature: PermissionFeature, granted: Boolean) {
        val event = when (feature) {
            PermissionFeature.MICROPHONE -> if (granted) ConsentEvent.MIC_PERMISSION_GRANTED else ConsentEvent.MIC_PERMISSION_DENIED
            PermissionFeature.CAMERA -> if (granted) ConsentEvent.CAMERA_PERMISSION_GRANTED else ConsentEvent.CAMERA_PERMISSION_DENIED
            PermissionFeature.NOTIFICATIONS -> if (granted) ConsentEvent.NOTIFICATIONS_GRANTED else ConsentEvent.NOTIFICATIONS_DENIED
            // Location does not yet have a ConsentEvent; Android remains the grant authority.
            PermissionFeature.APPROXIMATE_LOCATION -> return
        }
        consentLog.record(event, "permission_guide")
    }
}
