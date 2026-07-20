package ninja.jeremy.liveninja.log

import android.util.Log

/**
 * Drop-in facade for [android.util.Log] with an added [LogCategory] axis and
 * a pluggable [LogSink] backend (04-logging-delivery §A1). Every call is a
 * straight import-swap at existing `Log.d(TAG, "...")` call sites — just add
 * the category: `LNLog.d(LogCategory.WAKE, TAG, "...")`.
 *
 * Always passes through to logcat first (so `adb logcat` keeps working
 * unmodified), then — if a [sink] is registered — forwards the same entry to
 * it for redaction ([Redactor]), ring-buffering, and rotating-file
 * persistence.
 *
 * [sink] is `@Volatile var`, not constructor-injected: [LNLog] is a plain
 * `object` reachable from anywhere (including code that runs before Hilt's
 * graph is up), and [LogSink] self-registers into this field from its own
 * `init {}` the moment it's constructed (see LogSink.kt). Null ⇒ logcat
 * passthrough only — never a crash, never a dropped call.
 */
object LNLog {

    @Volatile
    var sink: LogSink? = null

    fun v(category: LogCategory, tag: String, msg: String, tr: Throwable? = null): Int =
        emit(LogLevel.VERBOSE, category, tag, msg, tr) { if (tr != null) Log.v(tag, msg, tr) else Log.v(tag, msg) }

    fun d(category: LogCategory, tag: String, msg: String, tr: Throwable? = null): Int =
        emit(LogLevel.DEBUG, category, tag, msg, tr) { if (tr != null) Log.d(tag, msg, tr) else Log.d(tag, msg) }

    fun i(category: LogCategory, tag: String, msg: String, tr: Throwable? = null): Int =
        emit(LogLevel.INFO, category, tag, msg, tr) { if (tr != null) Log.i(tag, msg, tr) else Log.i(tag, msg) }

    fun w(category: LogCategory, tag: String, msg: String, tr: Throwable? = null): Int =
        emit(LogLevel.WARN, category, tag, msg, tr) { if (tr != null) Log.w(tag, msg, tr) else Log.w(tag, msg) }

    fun e(category: LogCategory, tag: String, msg: String, tr: Throwable? = null): Int =
        emit(LogLevel.ERROR, category, tag, msg, tr) { if (tr != null) Log.e(tag, msg, tr) else Log.e(tag, msg) }

    fun wtf(category: LogCategory, tag: String, msg: String, tr: Throwable? = null): Int =
        emit(LogLevel.ASSERT, category, tag, msg, tr) { if (tr != null) Log.wtf(tag, msg, tr) else Log.wtf(tag, msg) }

    private inline fun emit(
        level: LogLevel,
        category: LogCategory,
        tag: String,
        msg: String,
        tr: Throwable?,
        logcat: () -> Int,
    ): Int {
        val result = logcat()
        sink?.log(level, category, tag, msg, tr)
        return result
    }
}
