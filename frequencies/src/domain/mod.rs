//! The Frequency as a social object, independent of any transport.
//!
//! Everything in here is pure: no database, no Redis, no network. The state
//! machine, the role matrix, the admission rules and the host-loss decision
//! are functions of their inputs, which is what lets them be pinned by tests
//! that cannot pass because a dependency happened to be reachable.

pub mod frequency;
pub mod lifecycle;
pub mod participant;
pub mod permissions;
pub mod role;
pub mod speaker_request;

pub use frequency::{Frequency, FrequencyState, ReplayStatus, Visibility};
pub use participant::Participant;
pub use role::{Action, FrequencyRole};
pub use speaker_request::{RequestStatus, SpeakerRequest};
