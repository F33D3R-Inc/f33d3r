//! Admission and continuity rules: who may enter, and what happens when the
//! host is gone.

use std::time::Duration;

use super::frequency::{Frequency, FrequencyState};
use super::role::FrequencyRole;

/// Why a Tune In was refused. Each is a stable machine code a caller renders.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum JoinRefusal {
    NotLive,
    Locked,
    Blocked,
    Full,
    SpeakersFull,
}

impl JoinRefusal {
    pub fn code(self) -> &'static str {
        match self {
            Self::NotLive => "not_live",
            Self::Locked => "locked",
            Self::Blocked => "blocked",
            Self::Full => "full",
            Self::SpeakersFull => "speakers_full",
        }
    }
}

/// What the person would be on entry, given what the roles table grants
/// them. The host is always the host; a granted co-host or speaker keeps that
/// role across a rejoin; everyone else is a listener.
pub fn role_on_entry(
    freq: &Frequency,
    pial: &str,
    granted: Option<FrequencyRole>,
) -> FrequencyRole {
    if freq.host_pial == pial {
        FrequencyRole::Host
    } else {
        match granted {
            Some(FrequencyRole::CoHost) => FrequencyRole::CoHost,
            Some(FrequencyRole::Speaker) => FrequencyRole::Speaker,
            _ => FrequencyRole::Listener,
        }
    }
}

/// The admission decision. The host is never refused by lock or capacity —
/// the host coming back is what unlocks a stuck session — but is refused by
/// state like anyone else.
pub fn admit(
    freq: &Frequency,
    role: FrequencyRole,
    blocked: bool,
    listeners_now: i64,
    speakers_now: i64,
) -> Result<(), JoinRefusal> {
    if !freq.state.accepts_joins() {
        return Err(JoinRefusal::NotLive);
    }
    if blocked {
        return Err(JoinRefusal::Blocked);
    }
    if role == FrequencyRole::Host {
        return Ok(());
    }
    if freq.locked && !role.moderates() {
        return Err(JoinRefusal::Locked);
    }
    if role.speaks() {
        if speakers_now >= freq.max_speakers as i64 {
            return Err(JoinRefusal::SpeakersFull);
        }
    } else if listeners_now >= freq.max_listeners as i64 {
        return Err(JoinRefusal::Full);
    }
    Ok(())
}

/// Whether a listener may be promoted to speaker right now.
pub fn can_add_speaker(freq: &Frequency, speakers_now: i64) -> bool {
    speakers_now < freq.max_speakers as i64
}

/// What a Frequency does when its host has been absent for `absent_for`.
///
/// Deterministic, per §27: while the grace period runs, wait; once it has
/// passed, a present co-host carries the session on, otherwise it ends with
/// reason `host_lost`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HostLossOutcome {
    KeepWaiting,
    ContinueUnderCoHost,
    EndFrequency,
}

pub fn on_host_absent(
    absent_for: Duration,
    grace: Duration,
    cohost_present: bool,
) -> HostLossOutcome {
    if absent_for < grace {
        HostLossOutcome::KeepWaiting
    } else if cohost_present {
        HostLossOutcome::ContinueUnderCoHost
    } else {
        HostLossOutcome::EndFrequency
    }
}

/// Whether a Frequency in `state` that has been there for `age` should be
/// reconciled on boot. `starting` that never became live within the timeout
/// failed; `ending` that never finished is completed.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Reconcile {
    Leave,
    FailStarting,
    CompleteEnding,
}

pub fn reconcile_on_boot(
    state: FrequencyState,
    age: Duration,
    starting_timeout: Duration,
) -> Reconcile {
    match state {
        FrequencyState::Starting if age >= starting_timeout => Reconcile::FailStarting,
        FrequencyState::Ending => Reconcile::CompleteEnding,
        _ => Reconcile::Leave,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Utc;
    use uuid::Uuid;

    fn freq(state: FrequencyState) -> Frequency {
        Frequency {
            id: Uuid::nil(),
            version: 1,
            host_pial: "pial:c0ffee00-0000-4000-8000-000000000001".into(),
            title: "t".into(),
            description: String::new(),
            state,
            visibility: super::super::frequency::Visibility::Public,
            language: "en".into(),
            adult_content: false,
            speaker_verity_min_tier: 0,
            scheduled_at: None,
            started_at: None,
            ended_at: None,
            end_reason: None,
            recording_enabled: false,
            replay_status: super::super::frequency::ReplayStatus::None,
            max_speakers: 2,
            max_listeners: 3,
            requests_open: true,
            locked: false,
            media_node: None,
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }

    const HOST: &str = "pial:c0ffee00-0000-4000-8000-000000000001";
    const OTHER: &str = "pial:c0ffee00-0000-4000-8000-000000000002";

    #[test]
    fn host_is_always_host_and_grants_survive_rejoin() {
        let f = freq(FrequencyState::Live);
        assert_eq!(role_on_entry(&f, HOST, None), FrequencyRole::Host);
        assert_eq!(
            role_on_entry(&f, HOST, Some(FrequencyRole::Speaker)),
            FrequencyRole::Host
        );
        assert_eq!(role_on_entry(&f, OTHER, None), FrequencyRole::Listener);
        assert_eq!(
            role_on_entry(&f, OTHER, Some(FrequencyRole::CoHost)),
            FrequencyRole::CoHost
        );
        assert_eq!(
            role_on_entry(&f, OTHER, Some(FrequencyRole::Speaker)),
            FrequencyRole::Speaker
        );
    }

    #[test]
    fn nobody_joins_a_frequency_that_is_not_live() {
        for s in FrequencyState::ALL
            .into_iter()
            .filter(|s| *s != FrequencyState::Live)
        {
            let f = freq(s);
            assert_eq!(
                admit(&f, FrequencyRole::Host, false, 0, 0),
                Err(JoinRefusal::NotLive),
                "{s}"
            );
        }
    }

    #[test]
    fn blocked_is_refused_before_anything_else() {
        let f = freq(FrequencyState::Live);
        assert_eq!(
            admit(&f, FrequencyRole::Listener, true, 0, 0),
            Err(JoinRefusal::Blocked)
        );
    }

    #[test]
    fn lock_keeps_listeners_out_but_not_moderators_or_the_host() {
        let mut f = freq(FrequencyState::Live);
        f.locked = true;
        assert_eq!(
            admit(&f, FrequencyRole::Listener, false, 0, 0),
            Err(JoinRefusal::Locked)
        );
        assert_eq!(
            admit(&f, FrequencyRole::Speaker, false, 0, 0),
            Err(JoinRefusal::Locked)
        );
        assert!(admit(&f, FrequencyRole::CoHost, false, 0, 0).is_ok());
        assert!(admit(&f, FrequencyRole::Host, false, 0, 0).is_ok());
    }

    #[test]
    fn capacity_is_per_lane() {
        let f = freq(FrequencyState::Live);
        assert!(admit(&f, FrequencyRole::Listener, false, 2, 0).is_ok());
        assert_eq!(
            admit(&f, FrequencyRole::Listener, false, 3, 0),
            Err(JoinRefusal::Full)
        );
        assert!(admit(&f, FrequencyRole::Speaker, false, 3, 1).is_ok());
        assert_eq!(
            admit(&f, FrequencyRole::Speaker, false, 0, 2),
            Err(JoinRefusal::SpeakersFull)
        );
        // The host is not counted against either lane.
        assert!(admit(&f, FrequencyRole::Host, false, 3, 2).is_ok());
    }

    #[test]
    fn host_loss_is_deterministic() {
        let grace = Duration::from_secs(120);
        assert_eq!(
            on_host_absent(Duration::from_secs(119), grace, false),
            HostLossOutcome::KeepWaiting
        );
        assert_eq!(
            on_host_absent(Duration::from_secs(119), grace, true),
            HostLossOutcome::KeepWaiting
        );
        assert_eq!(
            on_host_absent(Duration::from_secs(120), grace, true),
            HostLossOutcome::ContinueUnderCoHost
        );
        assert_eq!(
            on_host_absent(Duration::from_secs(120), grace, false),
            HostLossOutcome::EndFrequency
        );
    }

    #[test]
    fn boot_reconciliation_only_touches_starting_and_ending() {
        let t = Duration::from_secs(60);
        assert_eq!(
            reconcile_on_boot(FrequencyState::Starting, Duration::from_secs(5), t),
            Reconcile::Leave
        );
        assert_eq!(
            reconcile_on_boot(FrequencyState::Starting, Duration::from_secs(60), t),
            Reconcile::FailStarting
        );
        assert_eq!(
            reconcile_on_boot(FrequencyState::Ending, Duration::ZERO, t),
            Reconcile::CompleteEnding
        );
        for s in [
            FrequencyState::Live,
            FrequencyState::Ended,
            FrequencyState::Draft,
        ] {
            assert_eq!(
                reconcile_on_boot(s, Duration::from_secs(9999), t),
                Reconcile::Leave
            );
        }
    }
}
