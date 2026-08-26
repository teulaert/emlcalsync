package caldav

import (
	"net/url"
	"strings"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Preset is what one CalDAV vendor needs that the protocol does not say.
//
// It lives in this package rather than in the CLI because every field is
// consumed somewhere the caller cannot reach: the credential text is formatted
// inside AuthError, five frames below any call site, and HomeFallback and
// PrimaryNames are used during discovery.
type Preset struct {
	// BaseURL is the DAV root discovery starts from.
	BaseURL string
	// CredentialURL is where the user creates the password, and
	// CredentialName is what that vendor calls it.
	CredentialURL  string
	CredentialName string
	// HomeFallback builds the conventional calendar home when discovery fails.
	// A nil fallback makes discovery mandatory: iCloud's home is
	// /<dsid>/calendars/ and the numeric dsid cannot be guessed, so a wrong
	// guess would look like an empty account rather than a failure.
	HomeFallback func(base *url.URL, user string) string
	// PrimarySegments are the last path segments the vendor gives its default
	// calendar. These are checked before PrimaryNames because the server
	// assigns them: a display name is user-editable and, on iCloud,
	// localized — a Dutch account's default calendar is called "Privé" while
	// still living at .../calendars/home/.
	PrimarySegments []string
	// PrimaryNames are the display names the vendor gives the default calendar.
	PrimaryNames []string
}

var presets = map[model.Vendor]Preset{
	model.VendorFastmail: {
		BaseURL:        "https://caldav.fastmail.com/dav/",
		CredentialURL:  "https://app.fastmail.com/settings/security/devices",
		CredentialName: "app password",
		HomeFallback: func(base *url.URL, user string) string {
			return strings.TrimSuffix(base.Path, "/") + "/calendars/user/" + user + "/"
		},
		PrimarySegments: []string{"Default"},
		PrimaryNames:    []string{"Calendar"},
	},
	model.VendorICloud: {
		BaseURL:         "https://caldav.icloud.com/",
		CredentialURL:   "https://account.apple.com/account/manage",
		CredentialName:  "app-specific password",
		HomeFallback:    nil, // discovery is mandatory
		PrimarySegments: []string{"home"},
		PrimaryNames:    []string{"Home", "Calendar"},
	},
}

// PresetFor returns the vendor's preset, and whether there is one. An unknown
// or empty vendor is not an error by itself — a self-hosted server is
// configured by base URL instead — so the zero Preset is usable.
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
