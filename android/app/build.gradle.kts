plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.ksp)
    alias(libs.plugins.hilt)
}

val porcupineEnabled = (findProperty("liveninja.porcupine") as String?)?.toBoolean() == true

android {
    namespace = "ninja.jeremy.liveninja"
    compileSdk = 35

    defaultConfig {
        applicationId = "ninja.jeremy.liveninja"
        minSdk = 29
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"

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
        // Release keystore lives OUTSIDE the repo (C:\dev\live-ninja-keys\release.keystore).
        // Absent-safe: when the file is missing (CI), no release signing config is created
        // and assembleRelease produces an unsigned APK; assembleDebug is unaffected.
        val releaseKeystorePath = System.getenv("LIVENINJA_RELEASE_KEYSTORE")
            ?: (findProperty("liveninja.releaseKeystore") as String?)
            ?: "C:/dev/live-ninja-keys/release.keystore"
        val releaseKeystore = File(releaseKeystorePath)
        if (releaseKeystore.exists()) {
            create("release") {
                storeFile = releaseKeystore
                storePassword = System.getenv("LIVENINJA_RELEASE_STORE_PASSWORD") ?: "liveninja-release"
                keyAlias = System.getenv("LIVENINJA_RELEASE_KEY_ALIAS") ?: "liveninja"
                keyPassword = System.getenv("LIVENINJA_RELEASE_KEY_PASSWORD") ?: "liveninja-release"
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
