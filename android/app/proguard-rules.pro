# Keep WebRTC JNI surface — libwebrtc calls back into these classes by name.
-keep class org.webrtc.** { *; }
# ONNX Runtime JNI surface.
-keep class ai.onnxruntime.** { *; }
