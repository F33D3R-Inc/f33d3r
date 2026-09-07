package com.f33d3r.app.core

/**
 * A Fragment is a complete, server-rendered HTML snapshot of exactly one Facet.
 *
 * It is never a state delta and never a partial render: the server sends the whole
 * surface, and this client's only job is to put it where it belongs.
 *
 * [address] is the `data-facet-id` string exactly as the server wrote it, because the
 * server is the authority on where its own render belongs. [facetId] is that address
 * parsed into the canonical five-segment form when it is in that form. Some facets
 * this codebase already ships address themselves in shorter legacy forms
 * (`work:<id>`, `thread:<id>`, `msg:<id>`, `convo:<id>`, `composer:<id>`), so parsing
 * is a classification, never a precondition for delivery — a client that dropped
 * those would drop the timeline and the chat thread.
 */
data class Fragment(
    val address: String,
    val facetId: FacetId?,
    val html: String,
) {
    /** True when this fragment addresses the canonical five-segment facet id form. */
    val isCanonical: Boolean get() = facetId != null

    companion object {
        /**
         * Reads the `data-facet-id` attribute off the fragment's root element.
         *
         * The first occurrence is the root's by construction: a facet file has a single
         * root element and the server does not emit a fragment whose first tag is not
         * that root.
         */
        private val ROOT_FACET_ID =
            Regex("""data-facet-id\s*=\s*(?:"([^"]+)"|'([^']+)')""", RegexOption.IGNORE_CASE)

        /** Builds a Fragment from raw server HTML, or null when it addresses no facet. */
        fun of(html: String): Fragment? {
            val match = ROOT_FACET_ID.find(html) ?: return null
            val address = match.groupValues[1].ifEmpty { match.groupValues[2] }
            if (address.isEmpty()) return null
            return Fragment(address, FacetId.parse(address), html)
        }

        /**
         * Builds a Fragment addressed to [fallback] when the body declares no address.
         *
         * Used by the read lane, where the surface asked for a named facet and the
         * response body is that facet's render. A declared address always wins: the
         * server has just said where this belongs, and that beats what the caller asked.
         */
        fun of(fallback: String, html: String): Fragment =
            of(html) ?: Fragment(fallback, FacetId.parse(fallback), html)
    }
}
