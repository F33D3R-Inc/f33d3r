# The bridge the runtime calls into is reached by name from JavaScript, so R8 must
# not rename or remove it. Losing it does not fail the build — it fails every tap.
-keepclassmembers class com.f33d3r.app.ui.surface.FacetHost$Bridge {
    @android.webkit.JavascriptInterface <methods>;
}
-keepclassmembers class * {
    @android.webkit.JavascriptInterface <methods>;
}

# kotlinx.serialization keeps its generated serializers by reflection on the
# companion; the shrinker cannot see those references.
-keepattributes *Annotation*, InnerClasses
-dontnote kotlinx.serialization.**
-keepclassmembers class kotlinx.serialization.json.** {
    *** Companion;
}
-keepclasseswithmembers class kotlinx.serialization.json.** {
    kotlinx.serialization.KSerializer serializer(...);
}

# BouncyCastle registers algorithms reflectively. Only the parts the sealed lane
# uses are kept; the rest is allowed to be stripped.
-keep class org.bouncycastle.crypto.** { *; }
-keep class org.bouncycastle.jcajce.provider.** { *; }
-dontwarn org.bouncycastle.**

# OkHttp's optional platform integrations are absent on Android by design.
-dontwarn okhttp3.internal.platform.**
-dontwarn org.conscrypt.**
-dontwarn org.openjsse.**

# libwebrtc is reached through JNI by name; a renamed class is a native crash, not
# a compile error.
-keep class org.webrtc.** { *; }
-dontwarn org.webrtc.**
