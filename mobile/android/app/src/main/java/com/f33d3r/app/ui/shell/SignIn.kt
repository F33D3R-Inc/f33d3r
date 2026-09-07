package com.f33d3r.app.ui.shell

import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.net.LocalNetwork
import com.f33d3r.app.ui.ShellState
import com.f33d3r.app.ui.theme.Ink

/**
 * Sign-in.
 *
 * A handle and a password, and nothing else — no email, no phone number, no third
 * party. The password is used twice in this one moment: once to authenticate against
 * the server, and once, locally, to derive the key that unwraps this account's
 * messaging identity. It is not stored either time.
 */
@Composable
fun SignIn(
    state: ShellState,
    onSignIn: (handle: String, password: String) -> Unit,
    onOrigin: (String) -> Unit,
    origin: String,
) {
    var handle by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var editingOrigin by remember { mutableStateOf(false) }
    var originDraft by remember { mutableStateOf(origin) }

    // A local origin is behind a permission the platform asks for on the app's
    // behalf. The credentials wait for the answer; sign-in then proceeds either way,
    // and a refusal is reported by the server being unreachable, not guessed here.
    val context = LocalContext.current
    var awaiting by remember { mutableStateOf<Pair<String, String>?>(null) }
    val askLocalNetwork = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) {
        awaiting?.let { (h, p) -> onSignIn(h, p) }
        awaiting = null
    }
    val submit: (String, String) -> Unit = { h, p ->
        if (LocalNetwork.needsPermission(context, origin)) {
            awaiting = h to p
            askLocalNetwork.launch(LocalNetwork.PERMISSION)
        } else {
            onSignIn(h, p)
        }
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Ink.Surface)
            .imePadding(),
    ) {
        if (state.loading) {
            LinearProgressIndicator(
                color = Ink.Accent,
                trackColor = Ink.Hairline,
                modifier = Modifier.fillMaxWidth().height(2.dp).align(Alignment.TopCenter),
            )
        }

        Column(
            modifier = Modifier
                .align(Alignment.Center)
                .fillMaxWidth()
                .padding(horizontal = 24.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Wordmark()
            Spacer(Modifier.height(8.dp))
            Text(
                text = "Your identity is yours.",
                style = MaterialTheme.typography.bodyLarge,
                fontSize = 16.sp,
                color = Ink.Tertiary,
                textAlign = TextAlign.Center,
            )

            Spacer(Modifier.height(32.dp))
            Field(
                value = handle,
                onValueChange = { handle = it.trimStart().lowercase() },
                placeholder = "Handle",
                keyboard = KeyboardOptions(
                    capitalization = KeyboardCapitalization.None,
                    keyboardType = KeyboardType.Text,
                    imeAction = ImeAction.Next,
                ),
            )

            Spacer(Modifier.height(12.dp))
            Field(
                value = password,
                onValueChange = { password = it },
                placeholder = "Password",
                secret = true,
                keyboard = KeyboardOptions(
                    keyboardType = KeyboardType.Password,
                    imeAction = ImeAction.Done,
                ),
            )

            if (state.refusal.isNotEmpty()) {
                Spacer(Modifier.height(12.dp))
                Text(
                    text = state.refusal,
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    color = Ink.Refused,
                    textAlign = TextAlign.Center,
                    modifier = Modifier.fillMaxWidth(),
                )
            }

            Spacer(Modifier.height(20.dp))
            val ready = handle.isNotBlank() && password.isNotBlank() && !state.loading
            Box(
                modifier = Modifier
                    .fillMaxWidth()
                    .height(48.dp)
                    .clip(Pill)
                    .background(if (ready) Ink.Accent else Ink.Faint)
                    .clickable(enabled = ready) { submit(handle, password) },
                contentAlignment = Alignment.Center,
            ) {
                Text(
                    text = "Sign in",
                    style = MaterialTheme.typography.titleMedium,
                    fontSize = 15.sp,
                    fontWeight = FontWeight.Bold,
                    color = if (ready) Ink.OnAccent else Ink.Muted,
                )
            }

            Spacer(Modifier.height(24.dp))
            if (editingOrigin) {
                Field(
                    value = originDraft,
                    onValueChange = { originDraft = it.trim() },
                    placeholder = "https://f33d3r.com",
                    keyboard = KeyboardOptions(
                        keyboardType = KeyboardType.Uri,
                        imeAction = ImeAction.Done,
                    ),
                )
                Spacer(Modifier.height(12.dp))
                Text(
                    text = "Use this instance",
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    fontWeight = FontWeight.SemiBold,
                    color = Ink.Primary,
                    modifier = Modifier
                        .clickable {
                            onOrigin(originDraft)
                            editingOrigin = false
                        }
                        .padding(vertical = 8.dp),
                )
            } else {
                Text(
                    text = origin,
                    style = MaterialTheme.typography.bodyMedium,
                    fontSize = 14.sp,
                    color = Ink.Muted,
                    textAlign = TextAlign.Center,
                    modifier = Modifier
                        .clickable { editingOrigin = true }
                        .padding(vertical = 8.dp),
                )
            }
        }
    }
}

/** A 48-tall input on the raised ground, cut at 12, with no frame. */
@Composable
private fun Field(
    value: String,
    onValueChange: (String) -> Unit,
    placeholder: String,
    secret: Boolean = false,
    keyboard: KeyboardOptions = KeyboardOptions.Default,
) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(48.dp)
            .clip(RoundedCornerShape(12.dp))
            .background(Ink.Elevated)
            .padding(horizontal = 14.dp),
        contentAlignment = Alignment.CenterStart,
    ) {
        BasicTextField(
            value = value,
            onValueChange = onValueChange,
            singleLine = true,
            keyboardOptions = keyboard,
            visualTransformation = if (secret) PasswordVisualTransformation() else VisualTransformation.None,
            textStyle = MaterialTheme.typography.bodyLarge.copy(color = Ink.Primary, fontSize = 16.sp),
            cursorBrush = SolidColor(Ink.Accent),
            modifier = Modifier.fillMaxWidth(),
            decorationBox = { field ->
                if (value.isEmpty()) {
                    Text(
                        text = placeholder,
                        style = MaterialTheme.typography.bodyLarge,
                        fontSize = 16.sp,
                        color = Ink.Muted,
                    )
                }
                field()
            },
        )
    }
}
