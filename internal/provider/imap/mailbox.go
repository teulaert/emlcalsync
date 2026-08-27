package imap

import (
	"context"
	"sort"
	"strings"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
)

// folder is one mailbox as the server describes it, plus what we decided about
// it: its role, and whether it takes part in a sync.
type folder struct {
	Name   string
	Delim  rune
	Attrs  []imapv2.MailboxAttr
	Role   model.MailboxRole
	Status *imapv2.StatusData

	// Selectable is false for a \NoSelect placeholder: it is listed so its
	// children can find a parent, but it holds nothing.
	Selectable bool
	// Synced is false for a folder deliberately left out — \All, or Junk and
	// Trash when include_spam_trash is off.
	Synced bool
}

// Mailboxes returns the folder list.
func (m *Mail) Mailboxes(ctx context.Context) ([]model.Mailbox, error) {
	folders, err := m.listFolders(ctx)
	if err != nil {
		return nil, err
	}

	known := make(map[string]bool, len(folders))
	for name := range folders {
		known[name] = true
	}

	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]model.Mailbox, 0, len(names))
	for i, name := range names {
		f := folders[name]
		mb := model.Mailbox{
			RemoteID:     name, // the folder name IS the id: it is what SELECT takes
			Name:         leafName(name, f.Delim),
			Role:         f.Role,
			ParentRemote: parentOf(name, f.Delim, known),
			SortOrder:    i,
		}
		if s := f.Status; s != nil {
			if s.NumMessages != nil {
				mb.TotalCount = int(*s.NumMessages)
			}
			if s.NumUnseen != nil {
				mb.UnreadCount = int(*s.NumUnseen)
			}
		}
		out = append(out, mb)
	}
	return out, nil
}

// listFolders runs LIST (with STATUS where the server can fold it in) and
// applies the folder policy. The result is cached for role lookups.
func (m *Mail) listFolders(ctx context.Context) (map[string]folder, error) {
	var data []*imapv2.ListData

	err := m.pool.withAny(ctx, func(c *conn) error {
		opts := &imapv2.ListOptions{ReturnSpecialUse: hasCap(c.caps, imapv2.CapSpecialUse)}
		// One round trip instead of one per folder, where the server can.
		if hasCap(c.caps, imapv2.CapListStatus) || hasCap(c.caps, imapv2.CapIMAP4rev2) {
			opts.ReturnStatus = &imapv2.StatusOptions{
				NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true,
				HighestModSeq: hasCap(c.caps, imapv2.CapCondStore),
			}
		}
		var err error
		data, err = c.c.List("", "*", opts).Collect()
		return c.fail(wrapErr("list", err))
	})
	if err != nil {
		return nil, err
	}

	folders := make(map[string]folder, len(data))
	for _, d := range data {
		if hasAttr(d.Attrs, imapv2.MailboxAttrNonExistent) {
			continue
		}
		f := folder{
			Name:       d.Mailbox,
			Delim:      d.Delim,
			Attrs:      d.Attrs,
			Status:     d.Status,
			Selectable: !hasAttr(d.Attrs, imapv2.MailboxAttrNoSelect),
		}
		f.Role = m.roleFor(d.Mailbox, d.Delim, d.Attrs)
		f.Synced = f.Selectable && m.syncs(f)
		folders[d.Mailbox] = f
	}

	m.mu.Lock()
	m.boxes = folders
	m.mu.Unlock()
	return folders, nil
}

// folders returns the cached listing, refreshing it if nothing is cached yet.
func (m *Mail) folders(ctx context.Context) (map[string]folder, error) {
	m.mu.Lock()
	cached := m.boxes
	m.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	return m.listFolders(ctx)
}

// syncs applies the folder policy.
func (m *Mail) syncs(f folder) bool {
	if len(m.opts.Folders) > 0 {
		if !containsFold(m.opts.Folders, f.Name) {
			return false
		}
	}
	if containsFold(m.opts.ExcludeFolders, f.Name) || containsFold(m.preset.ExcludeFolders, f.Name) {
		return false
	}
	// \All holds a copy of every message in the account. With per-copy ids that
	// is the whole archive a second time, so it stays out unless asked for.
	if f.Role == model.RoleAll && !m.opts.IncludeAllMail {
		return false
	}
	if !m.opts.IncludeSpamTrash && (f.Role == model.RoleJunk || f.Role == model.RoleTrash) {
		return false
	}
	return true
}

// roleFor decides what a folder is for.
//
// SPECIAL-USE is authoritative where the server offers it. Otherwise the
// vendor's names, then the names servers converge on anyway, then the explicit
// config overrides — which always win, because they are the user telling us
// about a server we could not recognise.
func (m *Mail) roleFor(name string, delim rune, attrs []imapv2.MailboxAttr) model.MailboxRole {
	if strings.EqualFold(name, "INBOX") {
		return model.RoleInbox // SPECIAL-USE never marks the inbox
	}
	switch {
	case strings.EqualFold(name, m.opts.ArchiveFolder):
		return model.RoleArchive
	case strings.EqualFold(name, m.opts.SentFolder):
		return model.RoleSent
	case strings.EqualFold(name, m.opts.TrashFolder):
		return model.RoleTrash
	case strings.EqualFold(name, m.opts.DraftsFolder):
		return model.RoleDrafts
	}
	for _, a := range attrs {
		switch a {
		case imapv2.MailboxAttrArchive:
			return model.RoleArchive
		case imapv2.MailboxAttrDrafts:
			return model.RoleDrafts
		case imapv2.MailboxAttrJunk:
			return model.RoleJunk
		case imapv2.MailboxAttrSent:
			return model.RoleSent
		case imapv2.MailboxAttrTrash:
			return model.RoleTrash
		case imapv2.MailboxAttrAll:
			return model.RoleAll
		case imapv2.MailboxAttrImportant:
			return model.RoleImportant
			// \Flagged is deliberately not mapped. It is a virtual "starred"
			// view, not a folder — treating it as RoleImportant would let an
			// archive write into something that cannot hold messages.
		}
	}
	leaf := leafName(name, delim)
	for _, table := range []map[model.MailboxRole][]string{m.preset.RoleNames, genericRoleNames} {
		for role, names := range table {
			if containsFold(names, leaf) {
				return role
			}
		}
	}
	return ""
}

// roleRemote is the folder serving a role, or "" when the server has none.
func (m *Mail) roleRemote(ctx context.Context, role model.MailboxRole) (string, error) {
	folders, err := m.folders(ctx)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if folders[name].Role == role && folders[name].Selectable {
			return name, nil
		}
	}
	return "", nil
}

// syncedFolders is every folder that takes part in a sync, inbox first, then
// the roles, then the rest — so a long backfill archives this year's mail
// before 2005's.
func (m *Mail) syncedFolders(ctx context.Context) ([]folder, error) {
	return m.syncedFoldersFrom(m.folders(ctx))
}

// freshSyncedFolders re-lists first. A delta must use this: reading the cache
// would make a folder that appeared, vanished or was renamed invisible, which
// is exactly the thing the delta exists to notice.
func (m *Mail) freshSyncedFolders(ctx context.Context) ([]folder, error) {
	return m.syncedFoldersFrom(m.listFolders(ctx))
}

func (m *Mail) syncedFoldersFrom(all map[string]folder, err error) ([]folder, error) {
	if err != nil {
		return nil, err
	}
	var out []folder
	for _, f := range all {
		if f.Synced {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := rolePriority(out[i].Role), rolePriority(out[j].Role)
		if a != b {
			return a < b
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func rolePriority(r model.MailboxRole) int {
	switch r {
	case model.RoleInbox:
		return 0
	case model.RoleArchive:
		return 1
	case model.RoleSent:
		return 2
	case model.RoleDrafts:
		return 3
	case model.RoleJunk, model.RoleTrash:
		return 5
	}
	return 4
}

// parentOf is the folder's parent, but only when the parent is itself listed:
// a server that hides an intermediate level would otherwise leave children
// pointing at a mailbox row that does not exist.
func parentOf(name string, delim rune, known map[string]bool) string {
	if delim == 0 {
		return ""
	}
	i := strings.LastIndex(name, string(delim))
	if i <= 0 {
		return ""
	}
	parent := name[:i]
	if known[parent] {
		return parent
	}
	return ""
}

// leafName is the last path segment, which is what a user calls the folder.
func leafName(name string, delim rune) string {
	if delim == 0 {
		return name
	}
	if i := strings.LastIndex(name, string(delim)); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

func hasAttr(attrs []imapv2.MailboxAttr, want imapv2.MailboxAttr) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}

func containsFold(list []string, want string) bool {
	if want == "" {
		return false
	}
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), want) {
			return true
		}
	}
	return false
}
