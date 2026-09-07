package com.f33d3r.app.ui.shell

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.ui.compose.ApplyButton
import com.f33d3r.app.ui.compose.SheetCancel
import com.f33d3r.app.ui.compose.SheetField
import com.f33d3r.app.ui.compose.SheetTitle
import com.f33d3r.app.ui.compose.ShellSheet
import com.f33d3r.app.ui.theme.Ink

private enum class Reach(val label: String) { HANDLE("@handle"), NUMBER("F33D3R Number") }

/**
 * New message.
 *
 * Two ways to reach somebody, and only two: the name they go by here, or the Number
 * they handed you. No phone number, no email, no address book — the identity is
 * the platform's own root. The server answers both with what it decides: the
 * conversation, a request the other person will rule on, or a gate that says no
 * more than that.
 */
@Composable
fun NewMessageSheet(
    onDismiss: () -> Unit,
    onHandle: (String) -> Unit,
    onNumber: (number: String, capability: String, note: String) -> Unit,
) {
    var reach by remember { mutableStateOf(Reach.HANDLE) }
    var handle by remember { mutableStateOf("") }
    var number by remember { mutableStateOf("") }
    var capability by remember { mutableStateOf("") }
    var note by remember { mutableStateOf("") }

    val ready = when (reach) {
        Reach.HANDLE -> handle.trim().trimStart('@').isNotEmpty()
        Reach.NUMBER -> number.isNotBlank()
    }

    ShellSheet(onDismiss) {
        SheetTitle("New message")

        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Reach.entries.forEach { r ->
                Chip(label = r.label, filled = r == reach, onClick = { reach = r })
            }
        }
        Spacer(Modifier.height(12.dp))

        Column(Modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            when (reach) {
                Reach.HANDLE -> {
                    SheetField(
                        value = handle,
                        onValue = { handle = it.filter { c -> !c.isWhitespace() } },
                        placeholder = "Message @handle…",
                        keyboard = KeyboardOptions(capitalization = KeyboardCapitalization.None),
                    )
                    Note("If they do not accept messages from you yet, what you write is held and they are asked.")
                }

                Reach.NUMBER -> {
                    SheetField(
                        value = number,
                        onValue = { number = it },
                        placeholder = "Their F33D3R Number, like 0412 8837 2919",
                        keyboard = KeyboardOptions(keyboardType = KeyboardType.Number),
                    )
                    SheetField(
                        value = capability,
                        onValue = { capability = it.trim() },
                        placeholder = "Contact link, if they sent you one",
                        keyboard = KeyboardOptions(capitalization = KeyboardCapitalization.None),
                    )
                    SheetField(
                        value = note,
                        onValue = { if (it.length <= 280) note = it },
                        placeholder = "A note, if they review requests",
                        keyboard = KeyboardOptions(capitalization = KeyboardCapitalization.Sentences),
                    )
                    Note("A Number reaches a person under the rules they set for it, and tells you nothing else about them.")
                }
            }
        }

        Spacer(Modifier.height(20.dp))
        ApplyButton(
            label = if (reach == Reach.HANDLE) "Open" else "Reach them",
            enabled = ready,
        ) {
            when (reach) {
                Reach.HANDLE -> onHandle(handle)
                Reach.NUMBER -> onNumber(number, capability, note)
            }
        }
        SheetCancel(onClick = onDismiss)
    }
}

@Composable
private fun Note(text: String) {
    Text(
        text = text,
        style = MaterialTheme.typography.bodyMedium,
        fontSize = 14.sp,
        color = Ink.Tertiary,
    )
}
