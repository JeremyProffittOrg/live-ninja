package ninja.jeremy.liveninja.realtime

/** Serializes callback publication with release; a check alone leaves a reconnect race. */
internal class WebRtcCallbackFence {
    private val lock = Any()
    private var generation = 0L
    private var active = false

    fun begin(): Long = synchronized(lock) { active = true; ++generation }
    fun current(): Long = synchronized(lock) { generation }
    fun invalidate() = synchronized(lock) { active = false; generation++ }

    fun runIfCurrent(candidate: Long, action: () -> Unit): Boolean = synchronized(lock) {
        if (!active || candidate != generation) false else { action(); true }
    }
}
