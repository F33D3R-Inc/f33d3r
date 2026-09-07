use std::time::Instant;

use metrics::{counter, describe_counter, describe_gauge, describe_histogram, gauge, histogram};

// ── names ────────────────────────────────────────────────────────────────────
pub const FREQUENCIES_ACTIVE: &str = "frequencies_active";
pub const FREQUENCIES_CREATED_TOTAL: &str = "frequencies_created_total";
pub const FREQUENCIES_ENDED_TOTAL: &str = "frequencies_ended_total";

pub const PARTICIPANTS_CURRENT: &str = "frequency_participants_current";
pub const LISTENERS_CURRENT: &str = "frequency_listeners_current";
pub const SPEAKERS_CURRENT: &str = "frequency_speakers_current";

pub const JOIN_TOTAL: &str = "frequency_join_total";
pub const LEAVE_TOTAL: &str = "frequency_leave_total";
pub const JOIN_REFUSED_TOTAL: &str = "frequency_join_refused_total";
pub const SPEAKER_REQUESTS_TOTAL: &str = "frequency_speaker_requests_total";
pub const SPEAKER_PROMOTIONS_TOTAL: &str = "frequency_speaker_promotions_total";
pub const MODERATION_ACTIONS_TOTAL: &str = "frequency_moderation_actions_total";
pub const HOST_LOSS_TOTAL: &str = "frequency_host_loss_total";
pub const PRESENCE_SWEPT_TOTAL: &str = "frequency_presence_swept_total";
pub const RATE_LIMITED_TOTAL: &str = "frequency_rate_limited_total";
pub const IDEMPOTENT_REPLAY_TOTAL: &str = "frequency_idempotent_replay_total";
pub const VERSION_CONFLICT_TOTAL: &str = "frequency_version_conflict_total";

pub const EVENTS_PUBLISHED_TOTAL: &str = "frequency_events_published_total";
pub const EVENT_PUBLISH_FAILURES_TOTAL: &str = "frequency_event_publish_failures_total";
pub const EVENTS_UNPUBLISHED: &str = "frequency_events_unpublished";
pub const EVENT_PROPAGATION_SECONDS: &str = "frequency_event_propagation_seconds";

pub const HERALD_NOTIFY_TOTAL: &str = "frequency_herald_notify_total";

pub const DB_LATENCY_SECONDS: &str = "frequency_db_latency_seconds";
pub const REDIS_LATENCY_SECONDS: &str = "frequency_redis_latency_seconds";
pub const JOIN_AUTH_LATENCY_SECONDS: &str = "frequency_join_authorization_seconds";
pub const SPEAKER_PROMOTION_LATENCY_SECONDS: &str = "frequency_speaker_promotion_seconds";

pub const LEASE_HELD: &str = "frequency_leases_held";
pub const LEASE_LOST_TOTAL: &str = "frequency_lease_lost_total";

// Media-plane names, registered now so dashboards can be built before Phase 3
// fills them. They stay at zero until the SFU exists.
pub const WEBRTC_CONNECTIONS: &str = "frequency_webrtc_connections";
pub const WEBRTC_CONNECTION_FAILURES_TOTAL: &str = "frequency_webrtc_connection_failures_total";
pub const ICE_SUCCESS_TOTAL: &str = "frequency_ice_success_total";
pub const ICE_FAILURE_TOTAL: &str = "frequency_ice_failure_total";
pub const TURN_CONNECTIONS_CURRENT: &str = "frequency_turn_connections_current";
pub const RTP_PACKETS_RECEIVED_TOTAL: &str = "frequency_rtp_packets_received_total";
pub const RTP_PACKETS_FORWARDED_TOTAL: &str = "frequency_rtp_packets_forwarded_total";
pub const RTP_PACKETS_DROPPED_TOTAL: &str = "frequency_rtp_packets_dropped_total";
pub const AUDIO_BYTES_IN_TOTAL: &str = "frequency_audio_bytes_in_total";
pub const AUDIO_BYTES_OUT_TOTAL: &str = "frequency_audio_bytes_out_total";
pub const WEBSOCKET_CONNECTIONS: &str = "frequency_websocket_connections";
pub const WEBSOCKET_ERRORS_TOTAL: &str = "frequency_websocket_errors_total";
pub const RECONNECT_TOTAL: &str = "frequency_reconnect_total";
pub const RECORDING_JOBS: &str = "frequency_recording_jobs";
pub const RECORDING_FAILURES_TOTAL: &str = "frequency_recording_failures_total";

/// Describe every series once. Called after `observ::init` has installed the
/// recorder. Touching each series here also makes it appear on /metrics at
/// zero, so a dashboard panel is never empty for want of a first event.
pub fn describe() {
    describe_gauge!(
        FREQUENCIES_ACTIVE,
        "Frequencies currently live on this node"
    );
    describe_counter!(FREQUENCIES_CREATED_TOTAL, "Frequencies created");
    describe_counter!(FREQUENCIES_ENDED_TOTAL, "Frequencies ended, by reason");
    describe_gauge!(
        PARTICIPANTS_CURRENT,
        "Joined participants across live Frequencies"
    );
    describe_gauge!(
        LISTENERS_CURRENT,
        "Present listeners across live Frequencies"
    );
    describe_gauge!(SPEAKERS_CURRENT, "Present speakers across live Frequencies");
    describe_counter!(JOIN_TOTAL, "Tune In admitted, by role");
    describe_counter!(LEAVE_TOTAL, "Leaves, by cause");
    describe_counter!(JOIN_REFUSED_TOTAL, "Tune In refused, by reason");
    describe_counter!(SPEAKER_REQUESTS_TOTAL, "Request Mic, by outcome");
    describe_counter!(SPEAKER_PROMOTIONS_TOTAL, "Listener promoted to speaker");
    describe_counter!(MODERATION_ACTIONS_TOTAL, "Moderation actions, by action");
    describe_counter!(HOST_LOSS_TOTAL, "Host absence decisions, by outcome");
    describe_counter!(
        PRESENCE_SWEPT_TOTAL,
        "Presence sweeps: participants departed, or cold-Redis rounds skipped, by outcome"
    );
    describe_counter!(
        RATE_LIMITED_TOTAL,
        "Requests refused by rate limit, by scope"
    );
    describe_counter!(
        IDEMPOTENT_REPLAY_TOTAL,
        "Mutations answered from the idempotency store"
    );
    describe_counter!(
        VERSION_CONFLICT_TOTAL,
        "Compare-and-set conflicts, by operation"
    );
    describe_counter!(
        EVENTS_PUBLISHED_TOTAL,
        "Events published to Sitra Achra, by type"
    );
    describe_counter!(EVENT_PUBLISH_FAILURES_TOTAL, "Event publish failures");
    describe_gauge!(EVENTS_UNPUBLISHED, "Events waiting in the outbox");
    describe_histogram!(
        EVENT_PROPAGATION_SECONDS,
        "Seconds from event creation to broker ack"
    );
    describe_counter!(
        HERALD_NOTIFY_TOTAL,
        "Herald notifications, by type and outcome"
    );
    describe_histogram!(
        DB_LATENCY_SECONDS,
        "PostgreSQL operation latency, by operation"
    );
    describe_histogram!(
        REDIS_LATENCY_SECONDS,
        "Redis operation latency, by operation"
    );
    describe_histogram!(JOIN_AUTH_LATENCY_SECONDS, "Tune In authorization latency");
    describe_histogram!(
        SPEAKER_PROMOTION_LATENCY_SECONDS,
        "Approve-to-speaker latency"
    );
    describe_gauge!(LEASE_HELD, "Media-node leases this node holds");
    describe_counter!(LEASE_LOST_TOTAL, "Leases this node failed to renew");

    describe_gauge!(WEBRTC_CONNECTIONS, "Open WebRTC peer connections");
    describe_counter!(
        WEBRTC_CONNECTION_FAILURES_TOTAL,
        "WebRTC connection failures"
    );
    describe_counter!(ICE_SUCCESS_TOTAL, "ICE connected, by candidate pair type");
    describe_counter!(ICE_FAILURE_TOTAL, "ICE failed");
    describe_gauge!(TURN_CONNECTIONS_CURRENT, "Peers connected through a relay");
    describe_counter!(
        RTP_PACKETS_RECEIVED_TOTAL,
        "RTP packets received from speakers"
    );
    describe_counter!(
        RTP_PACKETS_FORWARDED_TOTAL,
        "RTP packets forwarded to listeners"
    );
    describe_counter!(RTP_PACKETS_DROPPED_TOTAL, "RTP packets dropped, by reason");
    describe_counter!(AUDIO_BYTES_IN_TOTAL, "Audio bytes received");
    describe_counter!(AUDIO_BYTES_OUT_TOTAL, "Audio bytes forwarded");
    describe_gauge!(WEBSOCKET_CONNECTIONS, "Open signaling sockets");
    describe_counter!(WEBSOCKET_ERRORS_TOTAL, "Signaling socket errors");
    describe_counter!(RECONNECT_TOTAL, "Participant reconnects");
    describe_gauge!(RECORDING_JOBS, "Recordings in progress");
    describe_counter!(RECORDING_FAILURES_TOTAL, "Recording failures");

    gauge!(FREQUENCIES_ACTIVE).set(0.0);
    gauge!(PARTICIPANTS_CURRENT).set(0.0);
    gauge!(LISTENERS_CURRENT).set(0.0);
    gauge!(SPEAKERS_CURRENT).set(0.0);
    gauge!(EVENTS_UNPUBLISHED).set(0.0);
    gauge!(LEASE_HELD).set(0.0);
    gauge!(WEBRTC_CONNECTIONS).set(0.0);
    gauge!(TURN_CONNECTIONS_CURRENT).set(0.0);
    gauge!(WEBSOCKET_CONNECTIONS).set(0.0);
    gauge!(RECORDING_JOBS).set(0.0);
    counter!(FREQUENCIES_CREATED_TOTAL).absolute(0);
    counter!(EVENT_PUBLISH_FAILURES_TOTAL).absolute(0);
    counter!(ICE_FAILURE_TOTAL).absolute(0);
    counter!(WEBRTC_CONNECTION_FAILURES_TOTAL).absolute(0);
}

/// A timer that records into a latency histogram with one fixed label.
pub struct Timed {
    name: &'static str,
    op: &'static str,
    start: Instant,
}

impl Timed {
    pub fn db(op: &'static str) -> Self {
        Self {
            name: DB_LATENCY_SECONDS,
            op,
            start: Instant::now(),
        }
    }

    pub fn redis(op: &'static str) -> Self {
        Self {
            name: REDIS_LATENCY_SECONDS,
            op,
            start: Instant::now(),
        }
    }

    pub fn named(name: &'static str, op: &'static str) -> Self {
        Self {
            name,
            op,
            start: Instant::now(),
        }
    }
}

impl Drop for Timed {
    fn drop(&mut self) {
        histogram!(self.name, "op" => self.op).record(self.start.elapsed().as_secs_f64());
    }
}
