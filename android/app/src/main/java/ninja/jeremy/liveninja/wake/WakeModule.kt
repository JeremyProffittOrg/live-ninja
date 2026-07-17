package ninja.jeremy.liveninja.wake

import dagger.Binds
import dagger.BindsOptionalOf
import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import dagger.multibindings.IntoMap
import dagger.multibindings.StringKey
import ninja.jeremy.liveninja.audio.WakeWordEngine

/**
 * Wake-stack DI wiring.
 *
 * Engines are a multibinding map keyed by `settings.schema.json#/properties/wakeEngine` enum
 * values. The default openWakeWord engine is always present; the optional Porcupine engine
 * (compiled OUT by default — enable with `-Pliveninja.porcupine=true`, see app/build.gradle.kts)
 * contributes its own `@IntoMap @StringKey("porcupine")` binding from `src/porcupine/`.
 *
 * [WakeTokenProvider] is optional: the auth feature binds it when sign-in lands; until then
 * [ModelManager] serves the packaged/cached model without backend sync.
 */
@Module
@InstallIn(SingletonComponent::class)
abstract class WakeModule {

    @Binds
    @IntoMap
    @StringKey(WakePreferences.ENGINE_OPENWAKEWORD)
    abstract fun bindOpenWakeWordEngine(engine: OpenWakeWordEngine): WakeWordEngine

    /** Default engine when callers want "the" engine rather than a specific one. */
    @Binds
    abstract fun bindDefaultEngine(engine: OpenWakeWordEngine): WakeWordEngine

    @BindsOptionalOf
    abstract fun optionalWakeTokenProvider(): WakeTokenProvider
}
