package com.f33d3r.app.core

/**
 * A Facet identifier in the one format the platform recognises:
 *
 *     facet:<namespace>:<type>:<entity_id>:<sub_id>
 *
 * Facets are the fundamental independently mutable rendering surfaces. Nothing in
 * this client invents an alternate identifier format: an id that does not parse is
 * an id the server did not mint, and a mutation carrying one is dropped rather than
 * guessed at. That refusal is what keeps a malformed frame from writing into the
 * wrong surface.
 */
data class FacetId(
    val namespace: String,
    val type: String,
    val entityId: String,
    val subId: String,
) {
    /** The canonical wire form. [toString] is the identifier — never a debug rendering. */
    override fun toString(): String = "facet:$namespace:$type:$entityId:$subId"

    companion object {
        private const val PREFIX = "facet"
        private const val SEGMENTS = 5

        /** Parses the canonical form, or null when [raw] is not a facet id. */
        fun parse(raw: String?): FacetId? {
            if (raw.isNullOrEmpty()) return null
            val parts = raw.split(':')
            if (parts.size != SEGMENTS) return null
            if (parts[0] != PREFIX) return null
            if (parts.any { it.isEmpty() }) return null
            return FacetId(
                namespace = parts[1],
                type = parts[2],
                entityId = parts[3],
                subId = parts[4],
            )
        }
    }
}
