package com.f33d3r.app.net.live

import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.core.Fragment
import com.f33d3r.app.net.Http
import com.f33d3r.app.net.Session
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import okhttp3.Request
import okhttp3.Response
import okhttp3.sse.EventSource
import okhttp3.sse.EventSourceListener
import okhttp3.sse.EventSources
import java.util.concurrent.atomic.AtomicReference

/** Whether the application currently exists. */
enum class LiveState { CONNECTING, LIVE, DROPPED }

/**
 * FA Live — the persistent connection, and therefore the application.
 *
 * The pages this client shows are not loaded and then updated; they exist
 * continuously through this stream. So the connection is not a feature of a screen
 * and does not belong to one: it is opened once for the signed-in session, it
 * survives every navigation, and it reconnects on its own when the network drops.
 *
 * Its only job is to receive what the server decided and hand it on. It classifies
 * each event into a [Mutation] using the server's own event vocabulary, and it makes
 * no other judgement — it never merges events, never re-orders them, never renders,
 * and never invents one the server did not send.
 */
class FaLive(
    private val session: Session,
    private val scope: CoroutineScope,
) {

    private val _mutations = MutableSharedFlow<Mutation>(
        replay = 0,
        extraBufferCapacity = MUTATION_BUFFER,
        onBufferOverflow = BufferOverflow.SUSPEND,
    )

    /** Every mutation the server has published to this session, in arrival order. */
    val mutations: SharedFlow<Mutation> = _mutations.asSharedFlow()

    private val _state = MutableStateFlow(LiveState.DROPPED)
    val state: StateFlow<LiveState> = _state.asStateFlow()

    private val source = AtomicReference<EventSource?>(null)
    private var pump: Job? = null

    /**
     * Opens the connection and keeps it open for as long as the session lasts.
     *
     * Calling this while already connected is a no-op rather than a second
     * connection: the server bounds live connections per account and evicts the
     * oldest, so a duplicate would silently kill the working one and deliver every
     * event twice until it did.
     */
    fun connect() {
        if (pump?.isActive == true) return
        pump = scope.launch { run() }
    }

    /** Closes the connection. The session survives; the stream does not. */
    fun disconnect() {
        pump?.cancel()
        pump = null
        source.getAndSet(null)?.cancel()
        _state.value = LiveState.DROPPED
        emit(Mutation.Signal(SignalKind.CONNECTION, LiveState.DROPPED.name))
    }

    private suspend fun run() {
        var backoffMillis = MIN_BACKOFF_MILLIS
        val client = Http.streamClient(session)
        val factory = EventSources.createFactory(client)

        while (scope.isActive) {
            if (session.token == null) {
                // No authority, no stream. Wait for a sign-in rather than hammering an
                // endpoint that will refuse every attempt.
                _state.value = LiveState.DROPPED
                delay(MIN_BACKOFF_MILLIS)
                continue
            }

            _state.value = LiveState.CONNECTING
            emit(Mutation.Signal(SignalKind.CONNECTION, LiveState.CONNECTING.name))

            val closed = kotlinx.coroutines.CompletableDeferred<Unit>()
            val request = Request.Builder()
                .url(session.origin.trimEnd('/') + Endpoints.LIVE_STREAM)
                .header("Accept", "text/event-stream")
                .header("Cache-Control", "no-cache")
                .build()

            val listener = object : EventSourceListener() {
                override fun onOpen(eventSource: EventSource, response: Response) {
                    _state.value = LiveState.LIVE
                    emit(Mutation.Signal(SignalKind.CONNECTION, LiveState.LIVE.name))
                }

                override fun onEvent(
                    eventSource: EventSource,
                    id: String?,
                    type: String?,
                    data: String,
                ) {
                    classify(type.orEmpty(), data)?.let(::emit)
                }

                override fun onClosed(eventSource: EventSource) {
                    if (!closed.isCompleted) closed.complete(Unit)
                }

                override fun onFailure(
                    eventSource: EventSource,
                    t: Throwable?,
                    response: Response?,
                ) {
                    if (!closed.isCompleted) closed.complete(Unit)
                }
            }

            val es = factory.newEventSource(request, listener)
            source.set(es)
            closed.await()
            es.cancel()
            source.compareAndSet(es, null)

            _state.value = LiveState.DROPPED
            emit(Mutation.Signal(SignalKind.CONNECTION, LiveState.DROPPED.name))

            // Capped exponential backoff. The server evicts the oldest connection when
            // an account opens too many, so a tight reconnect loop would evict the
            // device's own working stream on another screen.
            delay(backoffMillis)
            backoffMillis = (backoffMillis * 2).coerceAtMost(MAX_BACKOFF_MILLIS)
            if (_state.value == LiveState.LIVE) backoffMillis = MIN_BACKOFF_MILLIS
        }
    }

    private fun emit(mutation: Mutation) {
        scope.launch { _mutations.emit(mutation) }
    }

    /**
     * Maps one server event onto the mutation it represents.
     *
     * The event name is the server's word for what happened, so the mapping is a
     * lookup rather than an interpretation. An event this client does not know is
     * dropped: acting on an unrecognised name would mean guessing where its payload
     * belongs, and a guess writes a render into the wrong surface.
     */
    private fun classify(type: String, data: String): Mutation? {
        if (type.isEmpty()) return null

        // Signals: the payload is text for the Shell's own chrome, not a facet.
        when (type) {
            "notify" -> return Mutation.Signal(SignalKind.NOTIFY, data.trim())
            "notif_new" -> return Mutation.Signal(SignalKind.NOTIFY_ARRIVED, data.trim())
            "balance" -> return Mutation.Signal(SignalKind.BALANCE, data.trim())
            "post_deleted" -> {
                val workId = data.trim()
                return if (workId.isEmpty()) null else Mutation.Remove("work:$workId")
            }
        }

        val fragment = Fragment.of(data) ?: return null

        return when (type) {
            // A sealed bubble belongs at the end of its thread's message list.
            "gnosis_message" -> Mutation.Append(fragment, GNOSIS_MESSAGES)

            // A live chat line belongs at the end of the stream's chat log.
            "live_chat" -> Mutation.Append(fragment, LIVE_CHAT)

            // A newly published work is held back rather than inserted, so the timeline
            // does not move under the reader.
            "new_post" -> Mutation.Pending(fragment)

            // Everything else is a facet the server re-rendered in place.
            "post_engagement",
            "follow_state",
            "new_reply",
            "live_facet",
            // A Frequency facet (stage, dock, card, queue) the server re-rendered.
            "frequency_facet",
            "sports_facet",
            "price_update",
            "tip_received",
            "article_view",
            "analytics_tick",
            "leaderboard_refresh",
            "marketplace_purchase_delivered",
            "creator_welcome_banner",
            -> Mutation.Replace(fragment)

            else -> null
        }
    }

    companion object {
        /** The element the sealed-thread bubbles are appended into. */
        const val GNOSIS_MESSAGES = "gnosis-messages"

        /** The element a live stream's chat lines are appended into. */
        const val LIVE_CHAT = "live-chat-log"

        private const val MUTATION_BUFFER = 128
        private const val MIN_BACKOFF_MILLIS = 1_000L
        private const val MAX_BACKOFF_MILLIS = 30_000L
    }
}
