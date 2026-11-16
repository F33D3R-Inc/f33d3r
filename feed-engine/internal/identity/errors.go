package identity

import (
	"errors"
	"fmt"
)

// Errors.
//
// elohim-veni refuses deliberately with a typed body, {"error": code,
// "message": sentence}. Every such refusal becomes an *Error carrying the code,
// and every code a caller branches on has a sentinel here, so a call site reads
//
//	if errors.Is(err, identity.ErrTOTPRequired) { … }
//
// and never compares a status number or a string.
//
// Anything that is not a deliberate refusal — a dial failure, a timeout, a 5xx,
// a body that does not decode — is ErrUnavailable. The two are never confused:
// a refusal is an answer, an outage is the absence of one, and the session path
// fails closed on the second without ever treating it as the first.

// ErrUnavailable means the authority could not be asked or did not answer. It
// is never a decision about the person.
var ErrUnavailable = errors.New("identity: elohim-veni unavailable")

// Error is a typed refusal from elohim-veni.
type Error struct {
	Status  int    // HTTP status the authority answered with
	Code    string // machine-readable reason, e.g. "invalid_credentials"
	Message string // the authority's sentence, safe to show a person
	Op      string // which call was refused, for the log
}

func (e *Error) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("identity: %s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("identity: %s: %s (%d): %s", e.Op, e.Code, e.Status, e.Message)
}

// Is matches on the refusal code alone, so a sentinel matches every refusal
// that carries its code whatever status or sentence came with it.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code != "" && t.Code == e.Code
}

// refusal builds a sentinel. Sentinels carry only a code.
func refusal(code string) *Error { return &Error{Code: code} }

// The refusal codes of the contract.
var (
	ErrInvalidCredentials = refusal("invalid_credentials")
	ErrSuspended          = refusal("suspended")
	ErrTOTPRequired       = refusal("totp_required")
	ErrTOTPInvalid        = refusal("totp_invalid")
	ErrTOTPAlreadyEnabled = refusal("totp_already_enabled")
	ErrTOTPNotEnabled     = refusal("totp_not_enabled")
	ErrTOTPNotEnrolled    = refusal("totp_not_enrolled")
	ErrBackupCodeInvalid  = refusal("backup_code_invalid")
	ErrSessionInvalid     = refusal("invalid_session")
	ErrNotFound           = refusal("not_found")
	ErrHandleTaken        = refusal("handle_taken")
	ErrHandleReserved     = refusal("handle_reserved")
	ErrHandleInvalid      = refusal("invalid_handle")
	ErrWeakPassword       = refusal("weak_password")
	ErrWrongPassword      = refusal("wrong_password")
	ErrTooYoung           = refusal("too_young")
	ErrInvalidDOB         = refusal("invalid_date_of_birth")
	ErrFounderPermanent   = refusal("founder_role_permanent")
	ErrDOBLocked          = refusal("dob_locked")
	ErrAlreadyLinked      = refusal("already_linked")
	ErrNotSolo            = refusal("account_not_solo")
	ErrRecoveryExpired    = refusal("recovery_expired")
	ErrPasswordNotSet     = refusal("password_not_set")
)

// Refusal reports the typed refusal inside err, or nil when err is not one.
func Refusal(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
