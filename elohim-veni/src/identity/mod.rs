//! The identity authority: PIALs, accounts, credentials, sessions, capabilities,
//! trust, the identity ledger, PIAL assertion tokens and identity.events.
//!
//! Every write in this module happens inside one transaction together with its
//! ledger row and its identity.events outbox row: either the change, its audit
//! record and its publication all happened, or none of them did.

pub mod accounts;
pub mod credentials;
pub mod events;
pub mod http;
pub mod ledger;
pub mod maintenance;
pub mod pial;
pub mod relay;
pub mod sessions;
pub mod tokens;
pub mod totp;
pub mod view;
pub mod vocab;
