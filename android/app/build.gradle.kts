plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.ksp)
    alias(libs.plugins.hilt)
}

val porcupineEnabled = (findProperty("liveninja.porcupine") as String?)?.toBoolean() == true

// Gated arm64-only slim build for the DEBUG variant only (CI's -Pliveninja.arm64Only=true
// distributes a slim debug APK — see .github/workflows/android-release.yml). Left OFF by
// default so local/emulator debug builds (x86_64 emulator) stay all-ABI and avoid
// INSTALL_FAILED_NO_MATCHING_ABIS. Release does NOT read this flag (see buildTypes.release
// below, M22.3): arm64-only is unconditionally the release default now, because the owner's
// phone (and every other real device this ships to) is arm64-v8a, and x86/x86_64 native libs
// exist solely to support the emulator, which never runs a release build.
val arm64Only = (findProperty("liveninja.arm64Only") as String?)?.toBoolean() == true

android {
    namespace = "ninja.jeremy.liveninja"
    compileSdk = 35

    defaultConfig {
        applicationId = "ninja.jeremy.liveninja"
        minSdk = 29
        targetSdk = 35
        // 0.3.0 is the first build that can handle an Azure session: it reads
        // callsUrl off the wire and declares azure-direct in X-LN-Capabilities.
        // It matches the broker's azureMinimums["android"] entry, so this
        // client qualifies by version as well as by capability.
        versionCode = 6
        versionName = "0.3.0"
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"

        // Debug-only, opt-in slim filter (see the `arm64Only` comment above). Release's own
        // unconditional filter lives on buildTypes.release below, not here, so a plain
        // `assembleDebug`/emulator run never sees it.
        if (arm64Only) {
            ndk {
                abiFilters += "arm64-v8a"
            }
        }

        // True only when the optional Porcupine engine source set + dependency are compiled
        // in (-Pliveninja.porcupine=true). Lets the Settings engine picker hide/disable the
        // Porcupine option in default builds instead of offering a dead choice.
        buildConfigField("boolean", "PORCUPINE_ENABLED", porcupineEnabled.toString())
    }

    signingConfigs {
        // Local debug keystore (android/keystores/ is gitignored). When absent —
        // e.g. on CI — AGP falls back to its auto-generated ~/.android/debug.keystore,
        // so debug builds stay green without any repo secret.
        val localDebugKeystore = rootProject.file("keystores/debug.keystore")
        if (localDebugKeystore.exists()) {
            getByName("debug") {
                storeFile = localDebugKeystore
                storePassword = "android"
                keyAlias = "androiddebugkey"
                keyPassword = "android"
            }
        }
        // Release keystore lives OUTSIDE the repo for local owner builds and is decoded
        // into android/keystores only for the lifetime of the manual release CI job.
        // Passwords and alias have no source-code defaults. The signing config exists
        // only when the keystore and all inputs are supplied; release CI separately
        // verifies the resulting APK signature before it can publish anything.
        val releaseKeystorePath = System.getenv("LIVENINJA_RELEASE_KEYSTORE")
            ?: (findProperty("liveninja.releaseKeystore") as String?)
            ?: "C:/dev/live-ninja-keys/release.keystore"
        val releaseKeystore = File(releaseKeystorePath)
        val releaseStorePassword = System.getenv("LIVENINJA_RELEASE_STORE_PASSWORD")
        val releaseKeyAlias = System.getenv("LIVENINJA_RELEASE_KEY_ALIAS")
        val releaseKeyPassword = System.getenv("LIVENINJA_RELEASE_KEY_PASSWORD")
        if (
            releaseKeystore.exists() &&
            !releaseStorePassword.isNullOrBlank() &&
            !releaseKeyAlias.isNullOrBlank() &&
            !releaseKeyPassword.isNullOrBlank()
        ) {
            create("release") {
                storeFile = releaseKeystore
                storePassword = releaseStorePassword
                keyAlias = releaseKeyAlias
                keyPassword = releaseKeyPassword
            }
        }
    }

    buildTypes {
        debug {
            // signingConfig left as AGP's default debug config, which we override
            // above when the local keystore exists.
        }
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
            signingConfig = signingConfigs.findByName("release")
            // M22.3: arm64-only, unconditionally, for every shipped build — debug's
            // opt-in -Pliveninja.arm64Only flag above intentionally does NOT gate this.
            // onnxruntime + webrtc ship prebuilt .so for 4 ABIs each; this is the entire
            // reason a release build was 177 MB (256 MB post-M23.1 dependency bump) instead
            // of roughly a quarter of that. `BuildType` (like `defaultConfig`/product
            // flavors) carries its own `ndk.abiFilters` in AGP's unified VariantDimension
            // DSL, so this narrows the release variant specifically without touching debug.
            ndk {
                abiFilters += "arm64-v8a"
            }
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    buildFeatures {
        compose = true
        buildConfig = true
    }
    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
    }
    testOptions {
        unitTests {
            // android.util.Log etc. return defaults instead of throwing in
            // local JVM tests (RealtimeSessionCoordinator logs on warn paths).
            isReturnDefaultValues = true
        }
    }

    // Optional Porcupine wake engine (plan.md M4 §3.1) — COMPILED OUT by default because it
    // needs a per-user Picovoice AccessKey and proprietary native libs. Enable with
    //   ./gradlew assembleDebug -Pliveninja.porcupine=true
    // The src/porcupine/ source set contributes PorcupineWakeWordEngine + its Hilt module
    // (@StringKey("porcupine") into the same engine map); no main-source changes either way.
    if (porcupineEnabled) {
        sourceSets.getByName("main") {
            java.srcDir("src/porcupine/java")
        }
    }
}

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.activity.compose)
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.ui.graphics)
    implementation(libs.androidx.compose.ui.tooling.preview)
    implementation(libs.androidx.compose.material3)
    implementation(libs.androidx.compose.material.icons.extended)
    implementation(libs.androidx.navigation.compose)
    implementation(libs.androidx.hilt.navigation.compose)
    implementation(libs.androidx.browser)
    implementation(libs.androidx.security.crypto)
    implementation(libs.androidx.lifecycle.process)
    implementation(libs.androidx.lifecycle.runtime.compose)
    // WorkManager watchdog for the wake-word service (M8.4 reliability): a 15-min
    // periodic worker that posts a tap-to-resume notification if listening is
    // enabled in prefs but the FGS died. hilt-work/hilt-compiler wire the worker
    // through Hilt (HiltWorkerFactory, see LiveNinjaApplication).
    implementation(libs.androidx.work.runtime.ktx)
    implementation(libs.androidx.hilt.work)
    ksp(libs.androidx.hilt.compiler)
    implementation(libs.kotlinx.coroutines.android)
    implementation(libs.kotlinx.serialization.json)
    implementation(libs.okhttp)
    implementation(libs.retrofit)
    implementation(libs.retrofit.kotlinx.serialization)
    implementation(libs.webrtc.sdk)
    implementation(libs.onnxruntime.android)
    if (porcupineEnabled) {
        implementation("ai.picovoice:porcupine-android:3.0.1")
    }

    implementation(libs.hilt.android)
    ksp(libs.hilt.compiler)

    testImplementation(libs.junit)
    // Real org.json on the JVM so DataChannel event parsing is unit-testable
    // (the android.jar org.json stubs throw at runtime in local tests).
    testImplementation(libs.org.json)
    testImplementation(libs.mockk)
    testImplementation(libs.kotlinx.coroutines.test)
    androidTestImplementation(libs.androidx.junit)
    androidTestImplementation(libs.androidx.espresso.core)
    androidTestImplementation(platform(libs.androidx.compose.bom))
    androidTestImplementation(libs.androidx.compose.ui.test.junit4)
    debugImplementation(libs.androidx.compose.ui.tooling)
    debugImplementation(libs.androidx.compose.ui.test.manifest)
}
