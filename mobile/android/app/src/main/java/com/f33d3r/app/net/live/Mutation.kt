package com.f33d3r.app.net.live

import com.f33d3r.app.core.Fragment

/**
 * One thing that happened to one Facet, as the server decided it.
 *
 * The client's whole vocabulary for change is here. There is no "update the count"
 * case and no "recompute" case, because the server does not send those: it sends the
 * facet's new render, or it says a facet is gone, or it reports a signal the Shell
 * chrome displays. Anything the client would have to work out for itself is not in
 * this list, and that absence is the invariant.
 */
sealed interface Mutation {

    /** The address this mutation is aimed at, when it is aimed at a surface. */
    val address: String?

    /** Replace the facet at this address with its new render. The common case. */
    data class Replace(val fragment: Fragment) : Mutation {
        override val address: String get() = fragment.address
    }

    /**
     * Add a newly rendered facet to the end of the collection it belongs to.
     *
     * Used where the server renders a new member of a list it does not re-render whole
     * — a sealed message arriving in an open thread.
     */
    data class Append(val fragment: Fragment, val container: String) : Mutation {
        override val address: String get() = fragment.address
    }

    /**
     * Hold a newly rendered facet out of the surface until the reader asks for it.
     *
     * A timeline that reflowed under the reader's thumb every time someone posted
     * would be unreadable, so the server's render is kept and the Shell offers it.
     * The fragment is still the server's, unmodified, and is inserted verbatim.
     */
    data class Pending(val fragment: Fragment) : Mutation {
        override val address: String get() = fragment.address
    }

    /** The facet at this address no longer exists. */
    data class Remove(override val address: String) : Mutation

    /**
     * A value the Shell's own chrome shows: the notification badge, the AET balance.
     *
     * These are the few places the server sends text rather than a fragment, because
     * their surface is native chrome rather than a Facet. The client displays the text
     * and derives nothing from it.
     */
    data class Signal(val kind: SignalKind, val value: String) : Mutation {
        override val address: String? get() = null
    }
}

/** The signals the native Shell chrome renders. */
enum class SignalKind {
    /** Unread notification badge text, already formatted by the server ("", "3", "9+"). */
    NOTIFY,

    /** Unread count grew — the notifications wire should ask for its list again. */
    NOTIFY_ARRIVED,

    /** The account's AET balance, formatted by the ledger. */
    BALANCE,

    /** A sealed message arrived for a thread that is not open. */
    MESSAGE_ARRIVED,

    /** The stream's own state, for the connection indicator. */
    CONNECTION,
}
