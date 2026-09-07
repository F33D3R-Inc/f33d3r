package com.f33d3r.app.compose

import android.content.Context
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.serialization.builtins.ListSerializer
import kotlinx.serialization.json.Json

/**
 * The drafts this device holds.
 *
 * Drafts are the composer's memory between openings and nothing more: they are not
 * works, the server has never seen them, and signing out clears them with the rest
 * of the session. The list is kept newest first, the way the drafts sheet shows it.
 */
class DraftStore(context: Context) {

    private val prefs = context.applicationContext
        .getSharedPreferences("f33d3r.drafts", Context.MODE_PRIVATE)

    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }
    private val serializer = ListSerializer(WorkDraft.serializer())

    private val _drafts = MutableStateFlow(load())
    val drafts: StateFlow<List<WorkDraft>> = _drafts.asStateFlow()

    fun get(id: String): WorkDraft? = _drafts.value.firstOrNull { it.id == id }

    /** Keeps [draft], replacing an earlier save of the same draft. */
    fun save(draft: WorkDraft) {
        val stamped = draft.copy(savedAt = System.currentTimeMillis())
        val rest = _drafts.value.filterNot { it.id == draft.id }
        persist(listOf(stamped) + rest)
    }

    fun remove(id: String) {
        if (_drafts.value.none { it.id == id }) return
        persist(_drafts.value.filterNot { it.id == id })
    }

    fun clear() = persist(emptyList())

    private fun persist(list: List<WorkDraft>) {
        _drafts.value = list
        prefs.edit().putString(KEY, json.encodeToString(serializer, list)).apply()
    }

    private fun load(): List<WorkDraft> =
        runCatching {
            prefs.getString(KEY, null)?.let { json.decodeFromString(serializer, it) }
        }.getOrNull().orEmpty()

    private companion object {
        const val KEY = "drafts"
    }
}
