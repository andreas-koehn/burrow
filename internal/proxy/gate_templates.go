package proxy

import (
	"html/template"
)

// loginTmpl is the login form template rendered for GET and failed POST to
// /__burrow/login. Inputs:
//   - .ServiceLabel string — first DNS label of the destination service (may be empty)
//   - .Next string        — validated next URL (hidden field; empty falls back server-side)
//   - .AlertMessage string — non-empty triggers role="alert" error banner
//   - .Action string       — the form's target (/__burrow/login or /__burrow/handoff)
//   - .NextField string    — the hidden field's name (next or return)
//   - .Nonce string        — the handoff's nonce hash (hidden field), or empty
//
// html/template auto-escapes all interpolated values. No template.HTML or
// template.URL casts are used.
var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Sign in — Burrow</title>
</head>
<body>
  <main>
    <h1>Sign in to continue</h1>
    {{if .ServiceLabel}}<p>You are accessing <strong>{{.ServiceLabel}}</strong>.</p>{{end}}
    {{if .AlertMessage}}
    <p role="alert" style="color:red">{{.AlertMessage}}</p>
    {{end}}
    <form action="{{.Action}}" method="POST">
      <input type="hidden" name="{{.NextField}}" value="{{.Next}}">
      {{if .Nonce}}<input type="hidden" name="nonce" value="{{.Nonce}}">{{end}}
      <div>
        <label for="email">Email</label><br>
        <input id="email" type="email" name="email" required autocomplete="username">
      </div>
      <div>
        <label for="password">Password</label><br>
        <input id="password" type="password" name="password" required autocomplete="current-password">
      </div>
      <button type="submit">Sign in</button>
    </form>
  </main>
</body>
</html>
`))

// accessDeniedTmpl is the access-denied page rendered when a user is
// authenticated but their role is not in the service's access policy.
// Inputs:
//   - .UserEmail string   — the authenticated user's email
//   - .UserRole string    — the authenticated user's role
//   - .ServiceLabel string — the service label (first DNS label of destination)
//   - .LogoutAction string — the POST URL for the logout form
//
// html/template auto-escapes all interpolated values.
var accessDeniedTmpl = template.Must(template.New("access-denied").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Access denied — Burrow</title>
</head>
<body>
  <main>
    <h1>Access denied</h1>
    <p>You are signed in as <strong>{{.UserEmail}}</strong> (role: <em>{{.UserRole}}</em>),
    but your role can&#39;t access &#39;{{.ServiceLabel}}&#39;.</p>
    <p>Contact an administrator if you need access.</p>
    <form action="{{.LogoutAction}}" method="POST">
      <button type="submit">Sign out</button>
    </form>
  </main>
</body>
</html>
`))
