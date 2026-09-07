package com.f33d3r.app.ui.shell

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
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
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.f33d3r.app.ui.compose.ApplyButton
import com.f33d3r.app.ui.compose.SheetCancel
import com.f33d3r.app.ui.compose.SheetField
import com.f33d3r.app.ui.compose.SheetTitle
import com.f33d3r.app.ui.compose.ShellSheet
import com.f33d3r.app.ui.theme.Ink
import kotlin.math.roundToLong

/** The presets the web shell's tip modal offers, in AET. */
private val PRESETS = listOf(1, 5, 10, 50)

/**
 * The tip sheet.
 *
 * Money goes straight from the sender's wallet to the creator's — the sheet collects
 * the amount and nothing else. It draws no balance and no confirmation of its own:
 * the ledger's answer, in the ledger's words, is what the Shell reports back.
 */
@Composable
fun TipSheet(
    handle: String,
    onDismiss: () -> Unit,
    onSend: (hundredths: Long) -> Unit,
) {
    var custom by remember { mutableStateOf("") }
    var preset by remember { mutableStateOf(PRESETS[1]) }

    val amount: Double = custom.toDoubleOrNull()?.takeIf { it > 0 } ?: preset.toDouble()

    ShellSheet(onDismiss) {
        SheetTitle("Send a tip", subtitle = "To @$handle · straight to their wallet")

        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            PRESETS.forEach { value ->
                val on = custom.isEmpty() && preset == value
                Chip(label = "$value AET", filled = on, onClick = { preset = value; custom = "" })
            }
        }

        Spacer(Modifier.height(12.dp))
        SheetField(
            value = custom,
            onValue = { v -> custom = v.filter { it.isDigit() || it == '.' } },
            placeholder = "Custom amount",
            keyboard = KeyboardOptions(keyboardType = KeyboardType.Decimal),
            trailing = {
                Text(
                    text = "AET",
                    style = MaterialTheme.typography.labelLarge,
                    fontSize = 13.sp,
                    fontWeight = FontWeight.SemiBold,
                    color = Ink.Tertiary,
                )
            },
        )

        Spacer(Modifier.height(20.dp))
        ApplyButton("Send ${trimAmount(amount)} AET") { onSend((amount * 100).roundToLong()) }
        SheetCancel(onClick = onDismiss)
    }
}

private fun trimAmount(v: Double): String =
    if (v == v.toLong().toDouble()) v.toLong().toString() else String.format(java.util.Locale.US, "%.2f", v)
