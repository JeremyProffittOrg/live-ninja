package ninja.jeremy.liveninja.log

/**
 * One category per subsystem/package (04-logging-delivery §A3). Declared
 * per call site: `LNLog.d(LogCategory.WAKE, TAG, "...")`. The Diagnostics
 * settings section (M6.2) renders these eight as an on/off checkbox group.
 */
enum class LogCategory {
    WAKE,
    AUDIO,
    REALTIME,
    AUTH,
    TOOLS,
    UI,
    NET,
    GENERAL,
}
