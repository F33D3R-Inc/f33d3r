# F33D3R Event Taxonomy

All events produced to Kafka follow this taxonomy. Every event has an **envelope** (mandatory fields shared by all
events) and a **payload** (event-specific fields).

---

## Envelope (all events)

| Field            | Type     | Description                                             |
|------------------|----------|---------------------------------------------------------|
| `event_id`       | UUID     | Unique event identifier. Idempotency key for consumers. |
| `event_type`     | string   | Dot-notation topic: `<topic>.<action>`                  |
| `schema_version` | string   | Semantic version of this event's schema                 |
| `pial_id`        | UUID     | PIAL of the actor who caused this event                 |
| `timestamp`      | ISO 8601 | UTC timestamp of when the event occurred                |

---

## Topics and event types

### `identity.*` — Produced by Nantar, Elohim Veni

| Event type                    | Trigger                                           |
|-------------------------------|---------------------------------------------------|
| `identity.registered`         | New user completes onboarding, PIAL root created  |
| `identity.capability_changed` | Elohim Veni updates a capability state            |
| `identity.tombstoned`         | PIAL permanently deactivated (ban)                |
| `identity.account_bound`      | Account linked to existing PIAL (re-registration) |

### `content.*` — Produced by Nantar

| Event type                | Trigger                             |
|---------------------------|-------------------------------------|
| `content.post_created`    | Post or reply successfully inserted |
| `content.post_deleted`    | Post removed by user or admin       |
| `content.post_liked`      | Like action recorded                |
| `content.post_bookmarked` | Bookmark action recorded            |

### `ranking.*` — Produced by AethyrRank

| Event type                 | Trigger                                          |
|----------------------------|--------------------------------------------------|
| `ranking.feed_served`      | Ranked feed delivered to user (session metadata) |
| `ranking.model_checkpoint` | LinUCB model checkpointed (every 60s)            |
| `ranking.exploration_slot` | Exploration item shown to user                   |

### `messaging.*` — Produced by Vovin

| Event type                      | Trigger                          | Note                       |
|---------------------------------|----------------------------------|----------------------------|
| `messaging.message_sent`        | Encrypted message relayed        | Metadata only — no content |
| `messaging.identity_registered` | Public key registered with Vovin |                            |

### `safety.*` — Produced by Zodacare and Elohim Veni

| Event type                  | Trigger                                    |
|-----------------------------|--------------------------------------------|
| `safety.report_created`     | User reports content                       |
| `safety.risk_score_updated` | Zodacare recalculates risk score           |
| `safety.action`             | Capability changed as result of moderation |
| `safety.report_resolved`    | Admin resolves a report                    |

### `track.*` — Produced by Nantar (via Zior signals)

| Event type             | Trigger                                                 |
|------------------------|---------------------------------------------------------|
| `track.uploaded`       | Audio file uploaded and stored                          |
| `track.played`         | Play event recorded                                     |
| `track.liked`          | Like event recorded                                     |
| `track.signal_updated` | Zior updates topic_vector after behavioral accumulation |

### `frequency.*` — Produced by Auralis (F33D3R Frequencies)

Topic `frequency.events`, partitioned by `frequency_id` so one Frequency's events arrive in order. Every event
also carries `frequency_id` and `correlation_id` in its envelope, and its `payload` object. Schemas are
generated from the producer (`auralis --write-schemas events/schemas`) and pinned by a test.

| Event type                        | Trigger                                                 |
|-----------------------------------|---------------------------------------------------------|
| `frequency.created`               | Host created a Frequency (draft or scheduled)           |
| `frequency.scheduled`             | Scheduled time set or changed                           |
| `frequency.started`               | Host started it; the session is live                    |
| `frequency.ended`                 | Ended by the host, by host loss, or on reconciliation   |
| `frequency.cancelled`             | A draft/scheduled Frequency was cancelled               |
| `frequency.failed`                | Could not start, or died mid-session                    |
| `frequency.listener_joined`       | Listener admitted                                       |
| `frequency.listener_left`         | Listener left, or presence expired                      |
| `frequency.speaker_requested`     | Request Mic, with a reason                              |
| `frequency.speaker_approved`      | Moderator approved a request                            |
| `frequency.speaker_declined`      | Declined by a moderator, or withdrawn                   |
| `frequency.speaker_joined`        | A speaking role entered                                 |
| `frequency.speaker_left`          | A speaking role left, or was demoted                    |
| `frequency.speaker_muted`         | Mute state changed (self or moderator)                  |
| `frequency.host_changed`          | Host ownership transferred (reserved)                   |
| `frequency.cohost_added`          | Host delegated a co-host                                |
| `frequency.cohost_removed`        | Co-host delegation revoked                              |
| `frequency.participant_removed`   | Moderator removed someone                               |
| `frequency.participant_blocked`   | Moderator blocked someone from this Frequency           |
| `frequency.locked`                | Lock state changed                                      |
| `frequency.requests_toggled`      | Speaker requests opened/closed                          |
| `frequency.recording_started`     | Recording began (consent indicator shown)               |
| `frequency.recording_completed`   | Recording finished                                      |
| `frequency.replay_ready`          | Frequency Replay available                              |
| `frequency.moderation_terminated` | Terminated by moderation                                |

### `economy.*` — Produced by Ain Soph

| Event type                 | Trigger                                              |
|----------------------------|------------------------------------------------------|
| `economy.transfer`         | AET transfer completed (deposit/tip/purchase/reward) |
| `economy.milestone_reward` | Realm progression milestone bonus paid               |

---

## Consumer responsibilities

| Consumer    | Topics consumed           | Why                                         |
|-------------|---------------------------|---------------------------------------------|
| AethyrRank  | `content.*`, `safety.*`   | Index new content, update safety epsilon    |
| Zodacare    | `content.*`, `identity.*` | Monitor new content and accounts for risk   |
| Zior        | `content.*`               | Detect audio content for analysis           |
| Nantar      | `safety.*`, `economy.*`, `frequency.*` | Update capability cache, show notifications, render Frequency Facets over FA Live |
| Elohim Veni | `safety.*`, `identity.*`  | Keep capability state in sync               |

---

## Ordering guarantees

- Events within the same `pial_id` are guaranteed ordered (same Kafka partition key).
- Events across different `pial_id`s are not ordered relative to each other.
- All consumers must be idempotent on `event_id` (at-least-once delivery).

---

## Schema evolution rules

- Adding optional fields: allowed (backward compatible).
- Removing fields: not allowed (bump schema_version and run dual-version consumers).
- Changing field types: not allowed (create a new event type).
- Adding required fields: only with a new schema_version.

All schemas are registered in `aethyr-schema-registry` and validated at CI time by
`enforcement/contract-checker/event_schema_validator.py`.
