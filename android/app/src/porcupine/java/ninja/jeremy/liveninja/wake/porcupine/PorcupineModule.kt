package ninja.jeremy.liveninja.wake.porcupine

import dagger.Binds
import dagger.Module
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import dagger.multibindings.IntoMap
import dagger.multibindings.StringKey
import ninja.jeremy.liveninja.audio.WakeWordEngine
import ninja.jeremy.liveninja.wake.WakePreferences

/**
 * Contributes the optional Porcupine engine to the wake-engine multibinding map.
 * Only compiled when `-Pliveninja.porcupine=true` (see app/build.gradle.kts).
 */
@Module
@InstallIn(SingletonComponent::class)
abstract class PorcupineModule {

    @Binds
    @IntoMap
    @StringKey(WakePreferences.ENGINE_PORCUPINE)
    abstract fun bindPorcupineEngine(engine: PorcupineWakeWordEngine): WakeWordEngine
}
