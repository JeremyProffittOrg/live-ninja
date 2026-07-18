package ninja.jeremy.liveninja.ui.settings

import android.content.Context
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import org.json.JSONObject

/**
 * The user's most recent custom wake-word training job (M6 FR-K03).
 * Backend statuses: pending | training | ready | failed.
 */
data class CustomWakeJob(
    val id: String,
    val phrase: String,
    val engine: String,
    val status: String,
    val error: String? = null,
) {
    val inFlight: Boolean get() = status == "pending" || status == "training"
    val ready: Boolean get() = status == "ready"
}

/**
 * Local persistence for the in-flight/last custom wake-word training job so
 * status polling survives process death (training runs up to 20 minutes on
 * AWS Batch — far longer than any single app session). One job at a time is
 * enough UI-side: the backend enforces concurrency ≤2 and ≤3/day/user anyway,
 * and a ready job graduates into the wake-word catalog/combobox.
 */
@Singleton
class CustomWakeWordStore @Inject constructor(
    @ApplicationContext context: Context,
) {
    private val prefs = context.getSharedPreferences("custom_wakeword", Context.MODE_PRIVATE)

    fun load(): CustomWakeJob? {
        val raw = prefs.getString(KEY_JOB, null) ?: return null
        return runCatching {
            val json = JSONObject(raw)
            CustomWakeJob(
                id = json.getString("id"),
                phrase = json.getString("phrase"),
                engine = json.optString("engine", "openwakeword"),
                status = json.optString("status", "pending"),
                error = if (json.isNull("error")) null else json.optString("error"),
            )
        }.getOrNull()
    }

    fun save(job: CustomWakeJob) {
        val json = JSONObject()
            .put("id", job.id)
            .put("phrase", job.phrase)
            .put("engine", job.engine)
            .put("status", job.status)
            .put("error", job.error ?: JSONObject.NULL)
        prefs.edit().putString(KEY_JOB, json.toString()).apply()
    }

    fun clear() {
        prefs.edit().remove(KEY_JOB).apply()
    }

    private companion object {
        const val KEY_JOB = "job_v1"
    }
}
