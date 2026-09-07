package com.f33d3r.app.compose

/** One handle the server offered for an @-token being typed, as the server named it. */
data class Mention(
    val handle: String,
    val displayName: String,
    val avatarUrl: String,
)
