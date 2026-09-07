# Zior Engine — Music Cortex

The music intelligence brain for the F33D3R platform.

**Brain name:** Zior  
**Port:** 8082  
**Language:** Rust 1.95.0 (axum, tokio)

Zior transforms raw audio into structured psychological signals using the same 8-dimensional axis system that AethyrRank uses for user interest vectors. A track's `topic_vector` and a user's `interest_vector` live in the same space — dot product equals direct compatibility. No genre labels. Pure vector alignment.

---

## What Zior does

- Accepts audio uploads (MP3, WAV, FLAC, OGG, AAC, Opus, M4A, MP4, WebM, MOV — up to 120MB)
- Extracts spectral features (BPM, key, loudness, timbre, spectral flux)
- Maps those features onto the 8 psychological axes shared with AethyrRank
- Tracks behavioral engagement per track (play, skip, complete events)
- Updates `topic_vector` as behavioral signals accumulate
- Pushes signals to AethyrRank for ranking on `trending_music` surface
- Clusters tracks by vector similarity (micro-community detection)

---

## The 8 axes (internal — not shown to users)

Zior and AethyrRank share the same 8-dimensional psychological space. This is the architectural decision that makes music and posts rankable in the same system.

| Axis | Name | High when |
|------|------|-----------|
| 0 | Persona | Production polish, vocal clarity, craft visibility |
| 1 | Shadow | Minor mode, dark timbre, low valence, bass dominance |
| 2 | Agency | High BPM, loud, energetic, driving momentum |
| 3 | Integration | Tonal stability, harmonic resolution, structural clarity |
| 4 | Attachment | Vocals, intimacy, slow tempo, warm mids |
| 5 | Disruption | Spectral flux, dynamic contrast, genre-bending |
| 6 | Tension | Dissonance, unresolved harmony, minor mode |
| 7 | Release | Major key, drop payoff, catharsis, euphoria |

These axes are **internal only** — users never see them. The UI shows genres, moods, and play counts.

---

## API

### `GET /health`
```json
{"status": "ok", "service": "zior-engine"}
```

### `GET /v1/tracks` — paginated track list
### `GET /v1/tracks/by-genre?genre=hip-hop` — genre filter
### `GET /v1/tracks/trending` — trending tracks by engagement velocity

### `POST /v1/tracks/upload` — multipart/form-data

Fields:
- `audio` — audio file (MP3, WAV, FLAC, OGG, AAC, Opus, M4A, MP4, WebM, MOV — max 120MB, min 320kbps for distribution)
- `title` — track title
- `genre` — genre string
- `description` — optional description

> Note: In the platform, uploads go through Nantar (`POST /upload/track`) which saves the file and creates the track record in `f33d3r_feed`. Zior handles the audio analysis pipeline.

### `GET /v1/artist/:user_id/tracks` — artist's published tracks
### `POST /v1/artist/:user_id/publish` — make a track visible

### `GET /v1/artists/trending` — top creators by engagement

### `POST /v1/feedback/play` — record a play event
### `POST /v1/feedback/like` — record a like event

### `GET /v1/signals/:track_id` — WebSocket signal subscription

---

## Supported formats

| Format | Extension | Notes |
|--------|-----------|-------|
| MP3 | `.mp3` | Min 320kbps for commercial distribution |
| WAV | `.wav` | Lossless, large files |
| FLAC | `.flac` | Lossless compressed |
| OGG Vorbis | `.ogg` | Open format |
| AAC | `.aac` | High quality at low bitrate |
| Opus | `.opus` | Best for streaming |
| M4A | `.m4a` | Apple audio |
| MP4 video | `.mp4` | Music video support |
| WebM | `.webm` | Web video |
| QuickTime | `.mov` | Video |

**Max file size:** 120MB  
**Min bitrate for distribution:** 320kbps

---

## Genres

14 genres currently supported:
`hip-hop` · `electronic` · `r-b` · `pop` · `rock` · `jazz` · `classical` · `ambient` · `lo-fi` · `metal` · `country` · `gospel` · `other`

---

## Artist onboarding

Any F33D3R user with the `MUSIC_UPLOAD` capability (granted by default) can upload tracks via the Studio mode in the Music page (`/music` → "Your Studio" tab).

The Studio provides:
- Drag-and-drop or file picker upload
- Cover art upload
- Title, genre, description form
- Free stream or paid distribution pricing
- BMI/ASCAP licensing acknowledgement
- Upload progress bar
- Track management (play, delete)
- Artist stats (total tracks, plays, likes)

---

## Project structure

```
zior-engine/
├── src/
│   ├── audio/
│   │   ├── encoder.rs       ← Multi-format codec support
│   │   ├── analyzer.rs      ← Spectral analysis, tempo detection, timbre
│   │   └── metadata.rs      ← ID3/Vorbis tag extraction
│   ├── vector/
│   │   ├── embedding.rs     ← Audio → 128D vector space
│   │   ├── similarity.rs    ← Cosine similarity search
│   │   └── store.rs         ← In-memory vector database
│   ├── behavioral/
│   │   ├── events.rs        ← Play/like/skip event tracking
│   │   ├── velocity.rs      ← Engagement velocity per track
│   │   └── affinity.rs      ← User → genre affinity learning
│   ├── jung/
│   │   ├── archetype.rs     ← Track → 8-axis mapping
│   │   └── resonance.rs     ← User interest alignment scoring
│   ├── signals/
│   │   ├── emitter.rs       ← Event → AethyrRank feedback
│   │   └── types.rs         ← Signal protocol definitions
│   ├── store/tracks.rs      ← Track metadata store
│   ├── brain/
│   │   ├── client.rs        ← AethyrRank integration
│   │   └── heartbeat.rs     ← Periodic schema registry registration
│   ├── api/handlers.rs      ← All HTTP endpoints
│   └── main.rs              ← Server startup
└── docker/
    └── Dockerfile           ← rust:1.95.0-slim-bookworm builder
```

---

## Running locally

```bash
# Via root compose (recommended)
docker compose -f ../docker-compose.local.yml up -d zior-engine

# Local development (requires Rust 1.95.0)
RUST_LOG=info cargo run --release
```

---

## What Zior does NOT do

- Make ranking decisions (that is AethyrRank's job)
- Store user personal data (only track signals and engagement counts)
- Apply genre labels to ranking (everything is vector-based alignment)
- Decide what content is appropriate (Zodacare handles moderation)
- Stream audio to users (Nantar serves static audio files directly)
