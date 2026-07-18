package ninja.jeremy.liveninja.realtime

import dagger.Binds
import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import javax.inject.Qualifier
import javax.inject.Singleton
import ninja.jeremy.liveninja.ui.state.RealtimeSessionController

/** The client-direct WebRTC-to-OpenAI transport (openai-realtime / -mini). */
@Qualifier
@Retention(AnnotationRetention.BINARY)
annotation class OpenAiRealtimeTransport

/** The backend Nova Sonic bridge transport (nova-sonic, M12). */
@Qualifier
@Retention(AnnotationRetention.BINARY)
annotation class NovaSonicTransport

/** Hilt bindings for the realtime package (owned by the WebRTC workstream). */
@Module
@InstallIn(SingletonComponent::class)
abstract class RealtimeModule {

    // Both transports are bound as [RealtimeTransport] under distinct
    // qualifiers; [RealtimeSessionCoordinator] injects both and selects one
    // per session from the resolved `voiceEngine` pin (M12 FR-VE-03).
    @Binds
    @Singleton
    @OpenAiRealtimeTransport
    abstract fun bindOpenAiTransport(impl: WebRtcTransport): RealtimeTransport

    @Binds
    @Singleton
    @NovaSonicTransport
    abstract fun bindNovaTransport(impl: NovaBridgeTransport): RealtimeTransport

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
