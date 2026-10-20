package gmail

const Scope = "https://www.googleapis.com/auth/gmail.modify"

// AuthorizationScopes includes optional basic identity access for inbox naming.
// Mailbox access still requires only Scope; identity permission may be declined.
const AuthorizationScopes = "openid profile " + Scope
