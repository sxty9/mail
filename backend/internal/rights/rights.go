// Package rights enumerates the fine-grained rights maild declares to the holistic rights
// standard. Each constant is the Linux group backing one permission in permissions/mail.json
// — keep the two in sync. Enforcement uses auth.User.Can: isAdmin || group ∈ groups.
//
// read+send default to true (every provisioned user gets a working mailbox; privleg can
// revoke); admin+audit default to false (admin-only until privleg grants them).
package rights

const (
	// GroupRead backs mailbox:read — use (read + manage) your own mailbox.
	GroupRead = "hp_mail_read"
	// GroupSend backs mailbox:send — compose and send from your address.
	GroupSend = "hp_mail_send"
	// GroupAdmin backs admin:admin — administer other mailboxes, domains, quotas.
	GroupAdmin = "hp_mail_admin"
	// GroupAudit backs audit:audit — read mail delivery logs and audit trails.
	GroupAudit = "hp_mail_audit"
)
