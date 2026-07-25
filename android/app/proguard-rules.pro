# M22.3 (plan.md) — release build now actually ships isMinifyEnabled/isShrinkResources (see
# app/build.gradle.kts), so every reflection- or JNI-reached surface below MUST be listed
# explicitly. Getting one of these wrong produces a release build that *installs* fine and
# crashes or silently misbehaves the first time the affected path runs (a bad HTTP response
# shape, a missing serializer, a JNI UnsatisfiedLinkError deep in a session) — far worse than
# the extra few MB a broader keep costs. Prefer an over-broad keep to a clever narrow one here.

# ---- WebRTC (io.github.webrtc-sdk) ----
# libwebrtc's native layer calls back into these Java classes/fields by name (JNI); the
# prebuilt .so was NOT built against this specific R8 obfuscation mapping, so nothing under
# org.webrtc may be renamed or stripped.
-keep class org.webrtc.** { *; }
-dontwarn org.webrtc.**

# ---- ONNX Runtime (com.microsoft.onnxruntime, wake-word inference) ----
# Same story: the bundled libonnxruntime.so / libonnxruntime4j_jni.so JNI bridge resolves
# ai.onnxruntime.* classes/methods by name from native code.
-keep class ai.onnxruntime.** { *; }
-dontwarn ai.onnxruntime.**

# ---- Hilt / Dagger ----
# Hilt's generated components (Hilt_LiveNinjaApplication, per-entry-point *_HiltComponents,
# @Module/@Binds factories, etc.) reference each other only through ordinary compile-time
# bytecode calls, not reflection — R8 traces those references fine on its own, and
# hilt-android/hilt-work ship their own consumer proguard rules (merged automatically from
# the AAR) covering their internal aggregated-deps machinery. The one Hilt-adjacent risk
# specific to this app is WorkManager's own persistence: a scheduled PeriodicWorkRequest
# stores its worker's class name as a STRING in WorkManager's Room DB and reconstructs it
# with Class.forName() on the next process start (independent of Hilt's own multibinding
# lookup, which resolves the @AssistedFactory by class reference, not by that persisted
# string) — so the worker class itself must survive completely intact.
-keep class ninja.jeremy.liveninja.wake.WakeWatchdogWorker { *; }
-keep class ninja.jeremy.liveninja.wake.WakeWatchdogWorker$* { *; }

# ---- Retrofit (com.squareup.retrofit2) ----
# Retrofit builds a dynamic Proxy for LiveNinjaApi and inspects each method's generic return
# type + annotations via reflection at call time; these attributes must survive verbatim
# (Retrofit's own published consumer rules — square/retrofit#3751).
-keepattributes Signature, InnerClasses, EnclosingMethod
-keepattributes RuntimeVisibleAnnotations, RuntimeVisibleParameterAnnotations, AnnotationDefault
-if interface * { @retrofit2.http.* <methods>; }
-keep,allowobfuscation interface <1>
-keepclassmembers,allowshrinking,allowobfuscation interface * {
    @retrofit2.http.* <methods>;
}
-dontwarn retrofit2.KotlinExtensions
-dontwarn retrofit2.KotlinExtensions$*
-dontwarn org.codehaus.mojo.animal_sniffer.*
-dontwarn javax.annotation.**
-dontwarn kotlin.Unit

# ---- OkHttp (com.squareup.okhttp3) ----
# okhttp/okio ship their own consumer rules for the internals; the only gap is the optional
# TLS providers OkHttp probes for reflectively and tolerates the absence of (none are on
# this app's classpath).
-dontwarn okhttp3.internal.platform.**
-dontwarn org.conscrypt.**
-dontwarn org.bouncycastle.**
-dontwarn org.openjsse.**

# ---- Tink / androidx.security-crypto (TokenStore's EncryptedSharedPreferences) ----
# Without these four lines `assembleRelease` does not merely warn — R8 ABORTS the build
# ("Missing classes detected while running R8", 100+ referencing contexts inside
# com.google.crypto.tink.*). Tink is compiled against Error Prone's static-analysis
# annotations (@Immutable, @CanIgnoreReturnValue, @CheckReturnValue, @RestrictedApi), which
# are deliberately compile-time-only: com.google.errorprone:error_prone_annotations is not a
# transitive runtime dependency of security-crypto, so the classes genuinely do not exist on
# the release classpath and never will. Nothing reads them at runtime (they annotate methods,
# they are not looked up reflectively), so suppressing is correct rather than papering over —
# the alternative, adding error_prone_annotations as a real dependency, would ship dead
# classes to fix a warning. This is exactly the rule set R8 itself emits into
# app/build/outputs/mapping/release/missing_rules.txt.
-dontwarn com.google.errorprone.annotations.**

# ---- kotlinx.serialization (backend DTOs: ninja.jeremy.liveninja.net.*Dtos.kt) ----
# The Retrofit converter (converter-kotlinx-serialization) resolves each response/request
# type's KSerializer at call time via kotlinx.serialization's KType-reflection path
# (Json.serializersModule.serializer(KType)), which walks a class's Kotlin @Metadata to find
# its generated Companion.serializer()/$serializer — NOT a plain static reference R8 can
# trace as "used". Without these keeps a release build throws
# SerializationException: "Serializer for class X is not found" the first time any network
# call decodes a response (a defect that unit tests on the JVM classpath cannot catch, since
# no shrinking runs there) — verify with a real release-build network round trip, not just
# `go`-style unit tests.
-keepattributes *Annotation*, InnerClasses, Signature
-keep,includedescriptorclasses class ninja.jeremy.liveninja.net.**$$serializer { *; }
-keepclassmembers class ninja.jeremy.liveninja.net.** {
    *** Companion;
}
-keepclasseswithmembers class ninja.jeremy.liveninja.net.** {
    kotlinx.serialization.KSerializer serializer(...);
}
-keepclassmembers class kotlinx.serialization.json.** {
    *** Companion;
}
-keepclasseswithmembers class kotlinx.serialization.json.** {
    kotlinx.serialization.KSerializer serializer(...);
}
-dontnote kotlinx.serialization.AnnotationsKt
