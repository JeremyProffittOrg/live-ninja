package ninja.jeremy.liveninja.net

import dagger.Module
import dagger.Provides
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import javax.inject.Qualifier
import javax.inject.Singleton
import kotlinx.serialization.json.Json
import ninja.jeremy.liveninja.config.BackendConfig
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import retrofit2.Retrofit
import retrofit2.converter.kotlinx.serialization.asConverterFactory

/**
 * Marks the OkHttp client that attaches backend auth (Bearer + X-LN-Client)
 * and performs the 401 refresh-and-retry. The unqualified client from
 * AppModule stays credential-free (use it for OpenAI/realtime/model
 * downloads — anything that is not the Live Ninja backend).
 */
@Qualifier
@Retention(AnnotationRetention.BINARY)
annotation class AuthorizedClient

/**
 * Shared backend API wiring. Client choice (noted per plan): **Retrofit**
 * with the first-party kotlinx-serialization converter — declarative
 * interfaces the other feature packages can extend, on top of the OkHttp
 * client the project already carries for WebRTC signaling.
 */
@Module
@InstallIn(SingletonComponent::class)
object NetModule {

    @Provides
    @Singleton
    fun provideJson(): Json = Json {
        ignoreUnknownKeys = true
        explicitNulls = false
    }

    @Provides
    @Singleton
    @AuthorizedClient
    fun provideAuthorizedClient(
        base: OkHttpClient,
        authInterceptor: AuthInterceptor,
        tokenAuthenticator: TokenAuthenticator,
    ): OkHttpClient =
        base.newBuilder()
            .addInterceptor(authInterceptor)
            .authenticator(tokenAuthenticator)
            .build()

    @Provides
    @Singleton
    fun provideRetrofit(
        @AuthorizedClient client: OkHttpClient,
        json: Json,
    ): Retrofit =
        Retrofit.Builder()
            .baseUrl("${BackendConfig.BASE_URL}/")
            .client(client)
            .addConverterFactory(json.asConverterFactory("application/json".toMediaType()))
            .build()

    @Provides
    @Singleton
    fun provideLiveNinjaApi(retrofit: Retrofit): LiveNinjaApi =
        retrofit.create(LiveNinjaApi::class.java)
}
