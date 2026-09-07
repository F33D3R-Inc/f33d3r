//! Roles and the authorization matrix.
//!
//! Directive §14, as a function. UI hiding a button is not security; this is
//! what Auralis enforces, independently, on every request.
//!
//! ```text
//!                        Host   CoHost   Speaker   Listener
//! End Frequency           YES     NO        NO        NO
//! Add / remove co-host    YES     NO        NO        NO
//! Lock / requests toggle  YES     NO        NO        NO
//! Remove participant      YES     YES       NO        NO
//! Block participant       YES     YES       NO        NO
//! Approve / decline       YES     YES       NO        NO
//! Demote speaker          YES     YES       NO        NO
//! Mute other              YES     YES       NO        NO
//! Mute self               YES     YES       YES       NO
//! Invite speaker          YES     YES       NO        NO
//! Request microphone      N/A     N/A       N/A       YES
//! Speak                   YES     YES       YES       NO
//! Listen                  YES     YES       YES       YES
//! ```

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize, sqlx::Type)]
#[serde(rename_all = "snake_case")]
#[sqlx(type_name = "TEXT", rename_all = "snake_case")]
pub enum FrequencyRole {
    Host,
    CoHost,
    Speaker,
    Listener,
}

impl FrequencyRole {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Host => "host",
            Self::CoHost => "cohost",
            Self::Speaker => "speaker",
            Self::Listener => "listener",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "host" => Some(Self::Host),
            "cohost" => Some(Self::CoHost),
            "speaker" => Some(Self::Speaker),
            "listener" => Some(Self::Listener),
            _ => None,
        }
    }

    /// Ordering for "may act on": a moderator acts only on people who rank
    /// strictly below them. A co-host cannot remove the host or another
    /// co-host; the host cannot be removed by anyone (the host leaves, or
    /// ends).
    fn rank(self) -> u8 {
        match self {
            Self::Host => 3,
            Self::CoHost => 2,
            Self::Speaker => 1,
            Self::Listener => 0,
        }
    }

    pub fn outranks(self, other: FrequencyRole) -> bool {
        self.rank() > other.rank()
    }

    /// Whether this role publishes audio.
    pub fn speaks(self) -> bool {
        matches!(self, Self::Host | Self::CoHost | Self::Speaker)
    }

    pub fn moderates(self) -> bool {
        matches!(self, Self::Host | Self::CoHost)
    }
}

impl std::fmt::Display for FrequencyRole {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Action {
    EndFrequency,
    AddCoHost,
    RemoveCoHost,
    LockFrequency,
    ToggleRequests,
    RemoveParticipant,
    BlockParticipant,
    ApproveSpeaker,
    DeclineSpeaker,
    DemoteSpeaker,
    MuteOther,
    MuteSelf,
    /// Invites are a later phase (§16 "Later"); the matrix already answers
    /// for them so the rule is settled before the route exists.
    #[allow(dead_code)]
    InviteSpeaker,
    RequestMic,
    Speak,
    Listen,
}

/// The matrix. `true` means the role may attempt the action; whether the
/// specific target is permitted is [`FrequencyRole::outranks`]'s question.
pub fn may(role: FrequencyRole, action: Action) -> bool {
    use Action::*;
    use FrequencyRole::*;
    match action {
        EndFrequency | AddCoHost | RemoveCoHost | LockFrequency | ToggleRequests => role == Host,
        RemoveParticipant | BlockParticipant | ApproveSpeaker | DeclineSpeaker | DemoteSpeaker
        | MuteOther | InviteSpeaker => role.moderates(),
        MuteSelf | Speak => role.speaks(),
        RequestMic => role == Listener,
        Listen => true,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use Action::*;
    use FrequencyRole::*;

    const ROLES: [FrequencyRole; 4] = [Host, CoHost, Speaker, Listener];

    fn permitted(action: Action) -> Vec<FrequencyRole> {
        ROLES.into_iter().filter(|r| may(*r, action)).collect()
    }

    #[test]
    fn only_the_host_ends_or_delegates() {
        for a in [
            EndFrequency,
            AddCoHost,
            RemoveCoHost,
            LockFrequency,
            ToggleRequests,
        ] {
            assert_eq!(permitted(a), vec![Host], "{a:?}");
        }
    }

    #[test]
    fn moderation_is_host_and_cohost() {
        for a in [
            RemoveParticipant,
            BlockParticipant,
            ApproveSpeaker,
            DeclineSpeaker,
            DemoteSpeaker,
            MuteOther,
            InviteSpeaker,
        ] {
            assert_eq!(permitted(a), vec![Host, CoHost], "{a:?}");
        }
    }

    #[test]
    fn speaking_roles_may_mute_themselves_and_speak() {
        assert_eq!(permitted(MuteSelf), vec![Host, CoHost, Speaker]);
        assert_eq!(permitted(Speak), vec![Host, CoHost, Speaker]);
    }

    /// A listener asks for the microphone. A speaker already has it, and a
    /// request from a speaker would be a queue entry nobody can act on.
    #[test]
    fn only_listeners_request_the_mic() {
        assert_eq!(permitted(RequestMic), vec![Listener]);
    }

    #[test]
    fn everyone_listens() {
        assert_eq!(permitted(Listen), ROLES.to_vec());
    }

    /// The rank rule: a co-host cannot act on the host or a peer co-host.
    #[test]
    fn moderators_act_only_downward() {
        assert!(Host.outranks(CoHost));
        assert!(Host.outranks(Listener));
        assert!(CoHost.outranks(Speaker));
        assert!(CoHost.outranks(Listener));
        assert!(!CoHost.outranks(CoHost));
        assert!(!CoHost.outranks(Host));
        assert!(!Speaker.outranks(Listener) || !may(Speaker, RemoveParticipant));
    }

    /// A browser sending `{"role":"host"}` is meaningless because roles are
    /// parsed from strings this brain wrote, and the matrix is keyed on the
    /// stored role. This pins that the parse is exact and closed.
    #[test]
    fn role_parse_is_closed() {
        for r in ROLES {
            assert_eq!(FrequencyRole::parse(r.as_str()), Some(r));
        }
        assert_eq!(FrequencyRole::parse("Host"), None);
        assert_eq!(FrequencyRole::parse("admin"), None);
        assert_eq!(FrequencyRole::parse(""), None);
    }
}
