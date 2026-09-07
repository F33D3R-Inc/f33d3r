package com.f33d3r.app

import android.graphics.Color
import android.graphics.drawable.ColorDrawable
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.SystemBarStyle
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.graphics.toArgb
import androidx.core.splashscreen.SplashScreen.Companion.installSplashScreen
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.f33d3r.app.ui.ShellModel
import com.f33d3r.app.ui.shell.Shell
import com.f33d3r.app.ui.shell.SignIn
import com.f33d3r.app.ui.theme.F33d3rTheme
import com.f33d3r.app.ui.theme.Ink

/**
 * The one Activity.
 *
 * There is nothing for a second one to do: the Shell is persistent and the wires
 * inside it are surfaces, not screens. A stack of Activities would be a second
 * navigation model competing with the one the Shell already is.
 */
class MainActivity : ComponentActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        installSplashScreen()
        val runtime = (application as F33d3rApp).runtime
        // The palette is resolved before the first frame, from the mode this device
        // chose and the theme the persona was last known to have, so the window ground
        // and the system-bar glyphs follow the app's mode rather than the system's.
        Ink.apply(runtime.session.identity.value.theme, runtime.session.mode)
        dressWindow()
        super.onCreate(savedInstanceState)

        setContent {
            F33d3rTheme {
                // A change of mode re-dresses the bars and the ground beneath the content.
                LaunchedEffect(Ink.dark) { dressWindow() }
                val model: ShellModel = viewModel(factory = ShellModelFactory(runtime))
                val state by model.state.collectAsStateWithLifecycle()

                if (state.identity.isSignedIn) {
                    Shell(model = model, runtime = runtime)
                } else {
                    SignIn(
                        state = state,
                        onSignIn = model::signIn,
                        onOrigin = model::useOrigin,
                        origin = runtime.session.origin,
                    )
                }
            }
        }
    }

    /**
     * Transparent bars whose glyphs are light on the dark palette and dark on the
     * light one, and a window ground that is the palette's surface, so nothing
     * lighter shows through before or between frames.
     */
    private fun dressWindow() {
        val dark = Ink.dark
        enableEdgeToEdge(
            statusBarStyle = SystemBarStyle.auto(Color.TRANSPARENT, Color.TRANSPARENT) { dark },
            navigationBarStyle = SystemBarStyle.auto(Color.TRANSPARENT, Color.TRANSPARENT) { dark },
        )
        window.setBackgroundDrawable(ColorDrawable(Ink.Surface.toArgb()))
    }

    override fun onStart() {
        super.onStart()
        // The connection is the application, so it comes back with the window. It is
        // idempotent: an already-open stream is left alone rather than replaced, which
        // is what stops the server evicting this device's own live connection.
        (application as F33d3rApp).runtime.live.connect()
    }
}

private class ShellModelFactory(private val runtime: Runtime) : ViewModelProvider.Factory {
    @Suppress("UNCHECKED_CAST")
    override fun <T : ViewModel> create(modelClass: Class<T>): T = ShellModel(runtime) as T
}
