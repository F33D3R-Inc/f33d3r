package com.f33d3r.app.net

import com.f33d3r.app.core.ContentSource
import com.f33d3r.app.core.Endpoints
import com.f33d3r.app.core.Fragment
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.FormBody
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.asRequestBody
import okhttp3.RequestBody.Companion.toRequestBody

/** What came back from a lane call: a rendered facet, or the reason there wasn't one. */
sealed interface LaneResult {
    /** The server rendered the facet. */
    data class Rendered(val fragment: Fragment) : LaneResult

    /** The server accepted the intent and rendered nothing (204, or an empty body). */
    data object Accepted : LaneResult

    /** The caller is not signed in, or the session the server knew has ended. */
    data object Unauthorized : LaneResult

    /**
     * The server refused, and said why in text meant for a person. [body] is the
     * answer as written — a refusal is often the Facet re-rendered with the reason
     * inside it, and the Shell reads the reason out of that render.
     */
    data class Refused(val code: Int, val message: String, val body: String = "") : LaneResult

    /** The request never reached a server. */
    data class Unreachable(val cause: Throwable) : LaneResult
}

/**
 * The server's answer on a lane that speaks JSON: the signed-work lane, the media
 * uploads. Kept apart from [LaneResult] because a JSON body is not a render, and
 * labelling one as a fragment is how a receipt ends up drawn on the timeline.
 */
sealed interface JsonAnswer {
    data class Ok(val body: JsonElement) : JsonAnswer
    data object Unauthorized : JsonAnswer

    /** The server refused, in its own words. */
    data class Refused(val code: Int, val message: String) : JsonAnswer
    data class Unreachable(val cause: Throwable) : JsonAnswer
}

/**
 * The server's answer to "who is signed in?". Three answers, kept apart because the
 * Shell must do three different things: keep the identity, drop it, or wait.
 */
sealed interface WhoAmI {
    /** The server named the persona. */
    data class Known(val identity: Identity) : WhoAmI

    /** The server answered and named nobody: the session it knew has ended. */
    data object Nobody : WhoAmI

    /** No server answered, or one answered that it was broken. Nothing is known. */
    data object Unreachable : WhoAmI
}

/**
 * The client's two lanes onto the server.
 *
 * Read lane: ask for a facet, receive its render. Write lane: send an intent,
 * receive the render the server decided that intent produces. Both directions carry
 * rendered HTML, because that is what a Facet is — the client never asks the server
 * for state and never assembles a view out of one.
 */
class FacetClient(
    private val session: Session,
    private val http: OkHttpClient = Http.client(session),
) {

    private val json = Json { ignoreUnknownKeys = true; isLenient = true }

    /** Fetches the facet [source] names and returns its rendered Fragment. */
    suspend fun read(source: ContentSource): LaneResult = withContext(Dispatchers.IO) {
        val url = source.url(session.origin)
        call(
            Request.Builder()
                .url(url)
                .get()
                // A facet request. Page routes answer it with the page's body as one
                // facet rather than the whole page; without it the Shell would be
                // handed a second shell to put inside itself.
                .header("HX-Request", "true")
                .build(),
            fallbackAddress = source.path,
        )
    }

    /**
     * Sends a form intent on the write lane.
     *
     * The server owns what an intent means and what it renders; the client contributes
     * the fields and nothing else. There is no optimistic render here on purpose — a
     * surface that drew the result itself would be asserting an outcome the server has
     * not agreed to yet, and would be wrong every time the server refuses.
     */
    suspend fun submit(path: String, fields: Map<String, String>): LaneResult =
        withContext(Dispatchers.IO) {
            val body = FormBody.Builder().apply {
                fields.forEach { (k, v) -> add(k, v) }
            }.build()
            call(
                Request.Builder()
                    .url(session.origin.trimEnd('/') + path)
                    .post(body)
                    // Marks the call as a facet request so the server answers with a
                    // fragment rather than a redirect to a page shell this client has no
                    // use for.
                    .header("HX-Request", "true")
                    .build(),
                fallbackAddress = path,
            )
        }

    /**
     * Sends a form intent that carries one file, on the write lane.
     *
     * The file is streamed off disk rather than read into the heap: a captured frame
     * is small, but the lane is the same one a clip would take, and a clip is not.
     * Everything else about the call is [submit] — the server decides what the upload
     * means and renders the answer.
     */
    suspend fun submitMultipart(
        path: String,
        fields: Map<String, String>,
        file: java.io.File,
        fileField: String,
        filename: String,
        mimeType: String,
    ): LaneResult = withContext(Dispatchers.IO) {
        val body = okhttp3.MultipartBody.Builder()
            .setType(okhttp3.MultipartBody.FORM)
            .apply { fields.forEach { (k, v) -> addFormDataPart(k, v) } }
            .addFormDataPart(fileField, filename, file.asRequestBody(mimeType.toMediaType()))
            .build()
        call(
            Request.Builder()
                .url(session.origin.trimEnd('/') + path)
                .post(body)
                .header("HX-Request", "true")
                .build(),
            fallbackAddress = path,
        )
    }

    /** Sends a JSON intent on the write lane. */
    suspend fun submitJson(path: String, payload: String): LaneResult =
        withContext(Dispatchers.IO) {
            val body: RequestBody = payload.toRequestBody(JSON_MEDIA)
            call(
                Request.Builder()
                    .url(session.origin.trimEnd('/') + path)
                    .post(body)
                    .header("HX-Request", "true")
                    .build(),
                fallbackAddress = path,
            )
        }

    /** Reads a JSON document from the server, for the few lanes that speak JSON. */
    suspend fun readJson(path: String, query: Map<String, String> = emptyMap()): JsonElement? =
        withContext(Dispatchers.IO) {
            val url = ContentSource(path, query).url(session.origin)
            runCatching {
                http.newCall(Request.Builder().url(url).get().build()).execute().use { response ->
                    if (!response.isSuccessful) return@use null
                    val text = response.body.string()
                    if (text.isBlank()) null else json.parseToJsonElement(text)
                }
            }.getOrNull()
        }

    /** Posts a JSON document and returns the JSON the server answered with, if any. */
    suspend fun postJson(path: String, payload: String): JsonElement? =
        withContext(Dispatchers.IO) {
            runCatching {
                val request = Request.Builder()
                    .url(session.origin.trimEnd('/') + path)
                    .post(payload.toRequestBody(JSON_MEDIA))
                    .build()
                http.newCall(request).execute().use { response ->
                    if (!response.isSuccessful) return@use null
                    val text = response.body.string()
                    if (text.isBlank()) json.parseToJsonElement("{}") else json.parseToJsonElement(text)
                }
            }.getOrNull()
        }

    /**
     * Signs in and adopts the session the server mints.
     *
     * The password is sent once, to the endpoint that already owns credential
     * checking, and is never stored. What persists is the token the server returned,
     * which is the only thing the client is entitled to hold.
     */
    suspend fun signIn(handle: String, password: String): LaneResult =
        withContext(Dispatchers.IO) {
            val body = FormBody.Builder()
                .add("handle", handle.trim().lowercase())
                .add("password", password)
                .build()
            val request = Request.Builder()
                .url(session.origin.trimEnd('/') + Endpoints.LOGIN)
                .post(body)
                .build()
            runCatching {
                http.newCall(request).execute().use { response ->
                    val text = response.body.string()
                    when {
                        session.token != null -> LaneResult.Accepted
                        // The login page re-renders itself with the reason on failure, so
                        // a 200 with no cookie is a refusal, not a success.
                        else -> LaneResult.Refused(response.code, loginError(text))
                    }
                }
            }.getOrElse { LaneResult.Unreachable(it) }
        }

    /** Ends the session on the server, then locally. */
    suspend fun signOut() = withContext(Dispatchers.IO) {
        runCatching {
            http.newCall(
                Request.Builder().url(session.origin.trimEnd('/') + Endpoints.LOGOUT).get().build()
            ).execute().close()
        }
        session.forget()
    }

    /**
     * Reads the signed-in identity from the server.
     *
     * The three names come from the page shell's own meta tags — the same values the
     * web client reads — so the app and the browser agree on who is signed in without
     * this client inferring an identity from a token it cannot interpret.
     *
     * A server that cannot be reached, or answers that it is broken, has said nothing
     * about the session; only a server that answers and names nobody has ended it.
     * The two are kept apart because the Shell forgets a session on the second and
     * must never forget one on the first.
     */
    suspend fun whoAmI(): WhoAmI = withContext(Dispatchers.IO) {
        val response = runCatching {
            http.newCall(
                Request.Builder().url(session.origin.trimEnd('/') + Endpoints.ROOT).get().build()
            ).execute()
        }.getOrElse { return@withContext WhoAmI.Unreachable }
        val shell = response.use { r ->
            when {
                r.code >= 500 -> return@withContext WhoAmI.Unreachable
                !r.isSuccessful -> return@withContext WhoAmI.Nobody
                else -> r.body.string()
            }
        }
        // The shell links its stylesheets with the stamp they are cached under. The
        // surfaces link the same files, so they take the same stamp.
        ASSET_VERSION.find(shell)?.groupValues?.get(1)?.let { session.assetVersion = it }

        val handle = meta(shell, "f33d3r:handle").orEmpty()
        val account = meta(shell, "f33d3r:account").orEmpty()
        val pial = meta(shell, "f33d3r:pial").orEmpty()
        if (handle.isEmpty() || account.isEmpty()) return@withContext WhoAmI.Nobody

        // The same flags the web navigation gates on, stated by the server in the
        // shell it rendered — so the drawer here shows exactly the entries the web
        // sidebar would, without this client forming its own view of the account.
        val base = Identity(
            pial = pial,
            account = account,
            handle = handle,
            verified = meta(shell, "f33d3r:verified") == "1",
            creator = meta(shell, "f33d3r:creator") == "1",
            admin = meta(shell, "f33d3r:admin") == "1",
            official = meta(shell, "f33d3r:official").orEmpty(),
            // The accent theme the server stamps on its own body element: the same
            // value the browser's stylesheet keys on, read rather than guessed.
            theme = Regex("""<body[^>]*\sdata-theme="([^"]*)"""").find(shell)?.groupValues?.get(1).orEmpty(),
        )
        // The avatar is the profile's, by exact handle, on the lane that answers by
        // handle; the display name comes from the handle search when it answers.
        // Both are stated as absolute addresses, because the frame's own image
        // loader has no document origin to resolve a path against.
        val mini = (readJson(Endpoints.PROFILES_MINI, mapOf("handles" to handle)) as? JsonArray)
            ?.firstOrNull { it.jsonObject["handle"]?.jsonPrimitive?.content.equals(handle, ignoreCase = true) }
            ?.jsonObject
        val named = (readJson(Endpoints.USERS_SEARCH, mapOf("q" to handle)) as? JsonArray)
            ?.firstOrNull { it.jsonObject["handle"]?.jsonPrimitive?.content.equals(handle, ignoreCase = true) }
            ?.jsonObject

        WhoAmI.Known(
            base.copy(
                displayName = named?.get("display_name")?.jsonPrimitive?.content.orEmpty(),
                avatarUrl = absolute(mini?.get("avatar_url")?.jsonPrimitive?.content.orEmpty()),
            )
        )
    }

    /** A server path as an address the frame's image loader can fetch. */
    private fun absolute(url: String): String = when {
        url.isEmpty() -> ""
        url.startsWith("http://") || url.startsWith("https://") -> url
        else -> session.origin.trimEnd('/') + (if (url.startsWith("/")) url else "/$url")
    }

    /** Posts a JSON document on a lane that answers in JSON, keeping the server's refusal. */
    suspend fun exchangeJson(path: String, payload: String): JsonAnswer =
        withContext(Dispatchers.IO) {
            answer(
                Request.Builder()
                    .url(session.origin.trimEnd('/') + path)
                    .post(payload.toRequestBody(JSON_MEDIA))
                    .header("HX-Request", "true")
                    .build(),
            )
        }

    /**
     * Uploads one file to a lane that answers in JSON: where it was stored, or why not.
     * The server decides the file's type from its bytes, not from the name given here.
     */
    suspend fun uploadJson(
        path: String,
        fields: Map<String, String>,
        file: java.io.File,
        fileField: String,
        filename: String,
        mimeType: String,
    ): JsonAnswer = withContext(Dispatchers.IO) {
        val body = okhttp3.MultipartBody.Builder()
            .setType(okhttp3.MultipartBody.FORM)
            .apply { fields.forEach { (k, v) -> addFormDataPart(k, v) } }
            .addFormDataPart(fileField, filename, file.asRequestBody(mimeType.toMediaType()))
            .build()
        answer(
            Request.Builder()
                .url(session.origin.trimEnd('/') + path)
                .post(body)
                .header("HX-Request", "true")
                .build(),
        )
    }

    private fun answer(request: Request): JsonAnswer =
        runCatching {
            http.newCall(request).execute().use { response ->
                val text = response.body.string()
                when {
                    response.code == 401 || response.code == 403 && text.contains("unauthorized") ->
                        JsonAnswer.Unauthorized

                    !response.isSuccessful ->
                        JsonAnswer.Refused(response.code, plainText(text).ifBlank { response.message })

                    else -> JsonAnswer.Ok(
                        if (text.isBlank()) json.parseToJsonElement("{}") else json.parseToJsonElement(text),
                    )
                }
            }
        }.getOrElse { JsonAnswer.Unreachable(it) }

    private fun call(request: Request, fallbackAddress: String): LaneResult =
        runCatching {
            http.newCall(request).execute().use { response ->
                val text = response.body.string()
                when {
                    response.code == 401 || response.code == 403 && text.contains("unauthorized") ->
                        LaneResult.Unauthorized

                    !response.isSuccessful ->
                        // A facet caller is refused with a rendered reason. The Shell
                        // shows the reason in its own chrome, so the markup comes off.
                        LaneResult.Refused(response.code, plainText(text).ifBlank { response.message }, text)

                    text.isBlank() -> LaneResult.Accepted

                    else -> LaneResult.Rendered(Fragment.of(fallbackAddress, text))
                }
            }
        }.getOrElse { LaneResult.Unreachable(it) }

    private companion object {
        val JSON_MEDIA = "application/json; charset=utf-8".toMediaType()

        val ASSET_VERSION = Regex("""/static/css/styles\.css\?v=([A-Za-z0-9._-]+)""")

        val META = { name: String ->
            Regex(
                """<meta\s+name=["']${Regex.escape(name)}["']\s+content=["']([^"']*)["']""",
                RegexOption.IGNORE_CASE,
            )
        }

        fun meta(html: String, name: String): String? =
            META(name).find(html)?.groupValues?.get(1)?.takeIf { it.isNotEmpty() }

        /** The text of a rendered refusal, without the markup it was rendered in. */
        fun plainText(html: String): String =
            html.replace(Regex("<[^>]*>"), " ")
                .replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">")
                .replace("&quot;", "\"").replace("&#39;", "'")
                .replace(Regex("\\s+"), " ")
                .trim()

        /** Pulls the human-readable reason out of the login page the server re-rendered. */
        fun loginError(html: String): String {
            val error = Regex("""class="[^"]*\berror\b[^"]*"[^>]*>([^<]{3,200})<""")
                .find(html)?.groupValues?.get(1)?.trim()
            return error ?: "Sign-in was refused."
        }
    }
}
