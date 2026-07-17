package ninja.jeremy.liveninja.realtime

import dagger.Binds
import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import javax.inject.Singleton
import ninja.jeremy.liveninja.ui.state.RealtimeSessionController

/** Hilt bindings for the realtime package (owned by the WebRTC workstream). */
@Module
@InstallIn(SingletonComponent::class)
abstract class RealtimeModule {

    /** Default media transport: WebRTC to OpenAI Realtime (M12 Nova Sonic swaps here). */
    @Binds
    @Singleton
    abstract fun bindRealtimeTransport(impl: WebRtcTransport): RealtimeTransport

    /**
     * Fills the UI layer's `@BindsOptionalOf` seam (ui/state/UiSeams.kt) —
     * the conversation screen's Optional<RealtimeSessionController> is
     * present from this binding on.
     */
    @Binds
    @Singleton
    abstract fun bindRealtimeSessionController(
        impl: RealtimeSessionCoordinator,
    ): RealtimeSessionController
}
