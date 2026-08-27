package imap

import (
	"github.com/teulaert/emlcalsync/internal/model"
)

// Security is how a connection is protected.
type Security string

const (
	// SecTLS is implicit TLS, from the first byte (IMAP 993, SMTP 465).
	SecTLS Security = "tls"
	// SecStartTLS opens in the clear and upgrades (IMAP 143, SMTP 587).
	SecStartTLS Security = "starttls"
	// SecNone is no encryption at all. Only for a fake server on loopback;
	// New refuses it unless Insecure is set.
	SecNone Security = "none"
)

// Preset is what one IMAP vendor needs that the protocol does not say.
//
// It lives here rather than in the CLI for the same reason the CalDAV one does:
// the credential text is formatted inside AuthError, far below any call site,
// and the role names and connection cap are consumed during listing and
// pooling. A server with no preset is not an error — that is what an explicit
// host is for — so the zero Preset is usable.
type Preset struct {
	Host     string
	Port     int
	Security Security

	SMTPHost     string
	SMTPPort     int
	SMTPSecurity Security
	// SMTPAppendsToSent means the submission server files its own copy in the
	// Sent folder. When it does, appending ours would double every sent
	// message, so Send must not.
	SMTPAppendsToSent bool

	// CredentialURL is where the user creates the password, and CredentialName
	// is what that vendor calls it.
	CredentialURL  string
	CredentialName string

	// MaxConnections caps concurrent IMAP connections. Servers rate-limit per
	// account, and going over does not slow you down — it locks the account
	// out, including whatever else the user reads their mail with.
	MaxConnections int

	// RoleNames maps folder names onto roles for servers that do not advertise
	// SPECIAL-USE. Matched case-insensitively against the leaf segment.
	RoleNames map[model.MailboxRole][]string

	// ExcludeFolders are never enumerated, whatever they are called.
	ExcludeFolders []string
}

var presets = map[model.Vendor]Preset{
	model.VendorICloud: {
		Host: "imap.mail.me.com", Port: 993, Security: SecTLS,
		SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPSecurity: SecStartTLS,
		// iCloud's submission does not file a copy; the client is expected to.
		SMTPAppendsToSent: false,
		CredentialURL:     "https://account.apple.com/account/manage",
		CredentialName:    "app-specific password",
		// Apple is strict about concurrent connections per account and answers
		// an excess with a temporary lockout, so stay well under.
		MaxConnections: 4,
		RoleNames: map[model.MailboxRole][]string{
			model.RoleSent:    {"Sent Messages", "Sent"},
			model.RoleTrash:   {"Deleted Messages", "Trash"},
			model.RoleDrafts:  {"Drafts"},
			model.RoleJunk:    {"Junk"},
			model.RoleArchive: {"Archive"},
		},
	},
	model.VendorFastmail: {
		Host: "imap.fastmail.com", Port: 993, Security: SecTLS,
		SMTPHost: "smtp.fastmail.com", SMTPPort: 465, SMTPSecurity: SecTLS,
		// Fastmail files the sent copy itself.
		SMTPAppendsToSent: true,
		CredentialURL:     "https://app.fastmail.com/settings/security/devices",
		CredentialName:    "app password",
		MaxConnections:    4,
		RoleNames: map[model.MailboxRole][]string{
			model.RoleSent:    {"Sent", "Sent Items"},
			model.RoleTrash:   {"Trash"},
			model.RoleDrafts:  {"Drafts"},
			model.RoleJunk:    {"Spam", "Junk"},
			model.RoleArchive: {"Archive"},
		},
	},
}

// PresetFor returns the vendor's preset, and whether there is one.
func PresetFor(v model.Vendor) (Preset, bool) {
	p, ok := presets[v]
	return p, ok
}

// credentialPhrase names the credential in prose, defaulting to something
// truthful for a server with no preset.
func (p Preset) credentialPhrase() string {
	if p.CredentialName == "" {
		return "password"
	}
	return p.CredentialName
}

// genericRoleNames are the folder names servers converge on without
// SPECIAL-USE. Consulted after the vendor's own list.
var genericRoleNames = map[model.MailboxRole][]string{
	model.RoleSent:    {"Sent", "Sent Items", "Sent Messages", "Sent Mail"},
	model.RoleDrafts:  {"Drafts", "Draft"},
	model.RoleTrash:   {"Trash", "Deleted", "Deleted Items", "Deleted Messages"},
	model.RoleJunk:    {"Junk", "Spam", "Junk E-mail", "Bulk Mail"},
	model.RoleArchive: {"Archive", "Archives"},
	// "All Mail" is a Gmail-style everything-folder, not an archive. Calling it
	// one would sync a second copy of the entire account, which is precisely
	// what the \All exclusion exists to prevent -- and a server without
	// SPECIAL-USE is exactly where the name has to carry that meaning.
	model.RoleAll: {"All Mail", "All"},
}
