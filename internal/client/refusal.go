package client

import (
	"strconv"

	"github.com/ankoehn/burrow/internal/proto"
)

// The two moments at which a relay can refuse.
const (
	stageAuth     = "auth"
	stageRegister = "register"
)

// RefusedError is the relay's refusal of the sign-in or of a tunnel.
type RefusedError struct {
	// Code is one of the proto.Code… values. An older relay sends none; for
	// the refusals it knows the code is then derived from its text. "" when
	// the reason is not known.
	Code string
	// Message is the relay's reason, as it sent it. It comes from outside:
	// what prints it to a terminal removes control characters first.
	Message string
	// Auth is true when the sign-in was refused, false for a tunnel.
	Auth bool
}

// Error is the text the client has always logged for a refusal.
func (e *RefusedError) Error() string {
	if e.Auth {
		return "auth failed: " + e.Message
	}
	return "register failed: " + e.Message
}

// Final reports whether trying again cannot change the refusal: the token is
// not accepted, the client is too old, or what was asked for a service is not
// possible. Everything else (a taken port, a relay in trouble, a reason this
// client does not know) may pass.
func (e *RefusedError) Final() bool {
	switch e.Code {
	case proto.CodeInvalidToken, proto.CodeClientTooOld,
		proto.CodeSlugInvalid, proto.CodeSlugTaken, proto.CodeAccessInvalid,
		proto.CodeForbidden:
		return true
	}
	return false
}

func newRefused(stage, code, message string) *RefusedError {
	return &RefusedError{Code: refusalCode(stage, code, message), Message: message, Auth: stage == stageAuth}
}

// refusalCode returns the code of a refusal. A relay from before the codes
// sends its text alone; for a rejected token that text has always been the
// same.
func refusalCode(stage, code, message string) string {
	if code != "" {
		return code
	}
	if stage == stageAuth && message == "invalid token" {
		return proto.CodeInvalidToken
	}
	return ""
}

// AccessNotAppliedError ends Run when an access mode other than open was
// wished for an http service and the relay registered the tunnel without a
// word about it. That relay is older: it ignored the wish, and the service it
// made is open to anyone with the URL. The registration cannot be taken back
// from here, so the client closes the session at once instead of serving.
type AccessNotAppliedError struct {
	Name   string // the service
	Access string // the relay access mode that was wished for
	URL    string // the public address the relay reported; from outside, may be ""
	// Services is how many services the run has when it has more than one
	// (`burrow up`), and 0 otherwise. The session ends for all of them: with
	// more than one, none of them is served.
	Services int
}

func (e *AccessNotAppliedError) Error() string {
	s := "register failed: the relay did not apply access mode " + e.Access + " to service " + e.Name
	if e.Services > 1 {
		return s + "; none of the " + strconv.Itoa(e.Services) + " services is served"
	}
	return s + "; the service is not served"
}
