package ninja.jeremy.liveninja

import android.os.SystemClock
import android.util.Log
import androidx.test.platform.app.InstrumentationRegistry
import java.io.File
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import org.junit.runner.Description
import org.junit.runner.Result
import org.junit.runner.notification.Failure
import org.junit.runner.notification.RunListener

/** Test-only breadcrumbs: never changes ordering, filters tests, or swallows failures. */
class TestProgressListener : RunListener() {
    private val timer = Executors.newSingleThreadScheduledExecutor { task ->
        Thread(task, "test-progress-watchdog").apply { isDaemon = true }
    }
    private var pendingSnapshot: ScheduledFuture<*>? = null
    private var startedAt = 0L
    private val logFile get() = File(
        InstrumentationRegistry.getInstrumentation().targetContext.cacheDir,
        "test-progress.log",
    )

    override fun testRunStarted(description: Description) {
        logFile.writeText("")
        emit("RUN_START tests=${description.testCount()}")
    }

    override fun testStarted(description: Description) {
        pendingSnapshot?.cancel(false)
        startedAt = SystemClock.elapsedRealtime()
        val snapshotStartedAt = startedAt
        val identity = "${description.className}#${description.methodName}"
        emit("START $identity")
        // One bounded snapshot per slow test; the CI wrapper owns the hard process deadline.
        pendingSnapshot = timer.schedule({
            emit("SLOW $identity elapsedMs=${SystemClock.elapsedRealtime() - snapshotStartedAt}")
            Thread.getAllStackTraces().entries
                .sortedWith(compareBy({ it.key.name != "main" }, { it.key.name }))
                .take(16)
                .forEach { (thread, frames) ->
                    emit("THREAD ${thread.name} state=${thread.state}")
                    frames.take(24).forEach { emit("  at $it") }
                }
        }, 60, TimeUnit.SECONDS)
    }

    override fun testFailure(failure: Failure) {
        emit("FAIL ${failure.description.className}#${failure.description.methodName} type=${failure.exception.javaClass.name}")
    }

    override fun testFinished(description: Description) {
        pendingSnapshot?.cancel(false)
        emit("END ${description.className}#${description.methodName} elapsedMs=${SystemClock.elapsedRealtime() - startedAt}")
    }

    override fun testRunFinished(result: Result) {
        pendingSnapshot?.cancel(false)
        timer.shutdownNow()
        emit("RUN_END tests=${result.runCount} failures=${result.failureCount} ignored=${result.ignoreCount}")
    }

    @Synchronized
    private fun emit(message: String) {
        Log.i("LNTestProgress", message)
        logFile.appendText(message + "\n")
    }
}
