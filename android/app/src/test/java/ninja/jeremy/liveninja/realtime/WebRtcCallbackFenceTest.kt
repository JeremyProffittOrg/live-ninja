package ninja.jeremy.liveninja.realtime

import io.mockk.mockk
import java.nio.ByteBuffer
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.yield
import okhttp3.OkHttpClient
import org.junit.Assert.*
import org.junit.Test
import org.webrtc.DataChannel
import org.webrtc.PeerConnection

class WebRtcCallbackFenceTest {
    @Test fun invalidatedOrReplacedCallbacksCannotPublish() {
        val fence = WebRtcCallbackFence()
        val old = fence.begin()
        assertTrue(fence.runIfCurrent(old) {})
        fence.invalidate()
        assertFalse(fence.runIfCurrent(old) { fail("after release") })
        val next = fence.begin()
        assertFalse(fence.runIfCurrent(old) { fail("after replacement") })
        assertTrue(fence.runIfCurrent(next) {})
    }

    @Test fun releaseWaitsForAcceptedCallbackPublicationBeforeReplacement() {
        val fence = WebRtcCallbackFence()
        val old = fence.begin()
        val entered = CountDownLatch(1)
        val release = CountDownLatch(1)
        val pool = Executors.newFixedThreadPool(2)
        val publications = mutableListOf("before")
        try {
            val callback = pool.submit {
                fence.runIfCurrent(old) {
                    entered.countDown()
                    assertTrue(release.await(3, TimeUnit.SECONDS))
                    publications += "old-completed"
                }
            }
            assertTrue(entered.await(3, TimeUnit.SECONDS))
            val teardown = pool.submit<Long> { fence.invalidate(); fence.begin() }
            release.countDown()
            callback.get(3, TimeUnit.SECONDS)
            val next = teardown.get(3, TimeUnit.SECONDS)
            assertFalse(fence.runIfCurrent(old) { publications += "stale" })
            fence.runIfCurrent(next) { publications += "new" }
            assertEquals(listOf("before", "old-completed", "new"), publications)
        } finally { release.countDown(); pool.shutdownNow() }
    }

    @Test fun actualOldDataChannelObserverCannotEmitIntoNewSession() = runBlocking {
        val transport = WebRtcTransport(mockk(relaxed = true), OkHttpClient(), mockk(relaxed = true))
        val fence = transport.javaClass.getDeclaredField("callbackFence").let {
            it.isAccessible = true; it.get(transport) as WebRtcCallbackFence
        }
        fun observer(generation: Long): DataChannel.Observer {
            val type = Class.forName("ninja.jeremy.liveninja.realtime.WebRtcTransport\$DcObserver")
            val ctor = type.declaredConstructors.single().also { it.isAccessible = true }
            return ctor.newInstance(transport, mockk<DataChannel>(relaxed = true), generation) as DataChannel.Observer
        }
        fun message() = DataChannel.Buffer(ByteBuffer.wrap(
            """{"type":"response.function_call_arguments.done","call_id":"old","name":"job_list","arguments":"{}"}""".toByteArray()), false)
        val seen = mutableListOf<RealtimeEvent>()
        val collector = launch(start = CoroutineStart.UNDISPATCHED) { transport.events.collect { seen += it } }
        val old = observer(fence.begin())
        fence.invalidate()
        val fresh = observer(fence.begin())
        old.onMessage(message())
        yield()
        assertTrue(seen.isEmpty())
        fresh.onMessage(message())
        yield()
        assertEquals(1, seen.filterIsInstance<RealtimeEvent.FunctionCall>().size)
        collector.cancel()
    }

    @Test fun oldPeerObserverCannotCloseReplacementConnection() {
        val transport = WebRtcTransport(mockk(relaxed = true), OkHttpClient(), mockk(relaxed = true))
        val fence = transport.javaClass.getDeclaredField("callbackFence").let {
            it.isAccessible = true; it.get(transport) as WebRtcCallbackFence
        }
        val ctor = Class.forName("ninja.jeremy.liveninja.realtime.WebRtcTransport\$PcObserver")
            .declaredConstructors.single().also { it.isAccessible = true }
        val old = ctor.newInstance(transport, fence.begin()) as PeerConnection.Observer
        fence.invalidate()
        fence.begin()
        val state = transport.javaClass.getDeclaredField("_state").let {
            it.isAccessible = true
            @Suppress("UNCHECKED_CAST")
            (it.get(transport) as kotlinx.coroutines.flow.MutableStateFlow<TransportState>)
        }
        state.value = TransportState.CONNECTED
        old.onConnectionChange(PeerConnection.PeerConnectionState.CLOSED)
        assertEquals(TransportState.CONNECTED, state.value)
    }
}
