package com.f33d3r.app.ui.surface

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive

/**
 * What a person did inside a projected facet, on its way to the server.
 *
 * An intent is not a decision. It says which control was used and what the server
 * asked to be sent with it; what it means, and what should be rendered as a result,
 * is entirely the server's to determine. That is why nothing here carries a
 * predicted outcome.
 */
sealed interface FacetIntent {

    /** A read- or write-lane call the facet declared on itself. */
    data class Lane(
        val method: String,
        val path: String,
        val values: Map<String, String>,
        val target: String,
        val swap: String,
        val origin: String,
    ) : FacetIntent

    /** An internal link. Where it leads is the native Shell's decision. */
    data class Navigate(val path: String) : FacetIntent

    /** A sealed composer was submitted: the body must be sealed before it is sent. */
    data class Seal(
        val convoId: String,
        val body: String,
        val origin: String,
    ) : FacetIntent

    /**
     * A tip control was used. The web shell answers this control with its tip modal;
     * the native Shell answers it with its own sheet. The amount is not here — the
     * facet only names who the tip is for.
     */
    data class Tip(val handle: String) : FacetIntent

    /**
     * A facet asked for a toast. The words are the server's, rendered into the
     * fragment; the Shell shows them in its own notice slot.
     */
    data class Notice(val text: String) : FacetIntent

    /**
     * A card's Quote control was used. Only the work's id travels: the server
     * recalls the work and renders the quoted embed for the composer.
     */
    data class Quote(val workId: String) : FacetIntent

    /**
     * A card's Reply control was used. The parent's content id is read off the
     * card the server rendered, exactly as the web composer reads it, because a
     * reply is signed against that id.
     */
    data class Reply(val workId: String, val cid: String, val handle: String) : FacetIntent

    companion object {
        private val json = Json { ignoreUnknownKeys = true; isLenient = true }

        /** Reads an intent the runtime forwarded, or null when it is not one. */
        fun parse(raw: String): FacetIntent? = runCatching {
            val obj = json.parseToJsonElement(raw).jsonObject
            when (obj.text("kind")) {
                "navigate" -> obj.text("path")
                    .takeIf { it.isNotEmpty() }
                    ?.let(FacetIntent::Navigate)

                "seal" -> Seal(
                    convoId = obj.text("convo"),
                    body = obj.values()["body"].orEmpty(),
                    origin = obj.text("origin"),
                ).takeIf { it.convoId.isNotEmpty() && it.body.isNotBlank() }

                "lane" -> Lane(
                    method = obj.text("method").ifEmpty { "GET" },
                    path = obj.text("path"),
                    values = obj.values(),
                    target = obj.text("target"),
                    swap = obj.text("swap").ifEmpty { "innerHTML" },
                    origin = obj.text("origin"),
                ).takeIf { it.path.isNotEmpty() }

                "tip" -> obj.text("handle").trim().trimStart('@')
                    .takeIf { it.isNotEmpty() }
                    ?.let(FacetIntent::Tip)

                "notice" -> obj.text("text").trim()
                    .takeIf { it.isNotEmpty() }
                    ?.let(FacetIntent::Notice)

                "quote" -> obj.text("workId").trim()
                    .takeIf { it.isNotEmpty() }
                    ?.let(FacetIntent::Quote)

                "reply" -> Reply(
                    workId = obj.text("workId").trim(),
                    cid = obj.text("cid").trim(),
                    handle = obj.text("handle").trim().trimStart('@'),
                ).takeIf { it.workId.isNotEmpty() }

                else -> null
            }
        }.getOrNull()

        private fun JsonObject.text(key: String): String =
            this[key]?.jsonPrimitive?.content.orEmpty()

        private fun JsonObject.values(): Map<String, String> =
            (this["values"] as? JsonObject)
                ?.mapValues { it.value.jsonPrimitive.content }
                .orEmpty()
    }
}
