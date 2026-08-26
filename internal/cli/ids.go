package cli

import (
	"fmt"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
)

// IDGroup is a set of remote ids belonging to one account, in input order.
type IDGroup struct {
	Account string
	Remotes []string
}

// ParseMessageID parses "<account>:<remote>" and validates the account.
func (a *App) ParseMessageID(s string) (account, remote string, err error) {
	p, err := model.ParseID(s)
	if err != nil || p.Kind != model.KindMessage {
		return "", "", output.Errorf(output.ExitUsage, "not a message id: %q", s)
	}
	if _, err := a.ResolveAccount(p.Account); err != nil {
		return "", "", err
	}
	return p.Account, p.Remote, nil
}

// ParseEventID parses "<account>:c:<calendar>:<event>".
func (a *App) ParseEventID(s string) (account, calendar, remote string, err error) {
	p, err := model.ParseID(s)
	if err != nil || p.Kind != model.KindEvent {
		return "", "", "", output.Errorf(output.ExitUsage, "not an event id: %q", s)
	}
	if _, err := a.ResolveAccount(p.Account); err != nil {
		return "", "", "", err
	}
	return p.Account, p.Calendar, p.Remote, nil
}

// GroupMessageIDs groups public message ids by account, preserving the order
// in which accounts first appear.
func (a *App) GroupMessageIDs(ids []string) ([]IDGroup, error) {
	var groups []IDGroup
	index := map[string]int{}
	for _, id := range ids {
		acct, remote, err := a.ParseMessageID(id)
		if err != nil {
			return nil, err
		}
		i, ok := index[acct]
		if !ok {
			i = len(groups)
			index[acct] = i
			groups = append(groups, IDGroup{Account: acct})
		}
		groups[i].Remotes = append(groups[i].Remotes, remote)
	}
	if len(groups) == 0 {
		return nil, output.Errorf(output.ExitUsage, "at least one id is required")
	}
	return groups, nil
}

// PublicMessageID is a convenience for presentation structs.
func PublicMessageID(account, remote string) string { return model.MessagePublicID(account, remote) }

func errNotFound(what, id string) error {
	return output.Errorf(output.ExitNotFound, "%s %s: %w", what, id, fmt.Errorf("%w", model.ErrNotFound))
}
