package client

import "github.com/ankoehn/burrow/internal/proto"

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
		proto.CodeForbidden, proto.CodeHTTPNotEnabled:
		return true
	}
	return false
}

func newRefused(stage, code, message string) *RefusedError {
	return &RefusedError{Code: refusalCode(stage, code, message), Message: message, Auth: stage == stageAuth}
}

// refusalCode returns the code of a refusal. A relay from before the codes
// sends its text alone; these are the texts it has always used.
func refusalCode(stage, code, message string) string {
	if code != "" {
		return code
	}
	switch {
	case stage == stageAuth && message == "invalid token":
		return proto.CodeInvalidToken
	case stage == stageRegister && message == "http tunnels not configured":
		return proto.CodeHTTPNotEnabled
	}
	return ""
}
